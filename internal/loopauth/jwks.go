package loopauth

import (
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"regexp"
	"sort"
	"sync"
	"time"
)

// JWK is one public key of a trusted issuer, as written in loop_server.issuers
// (jwks) or served by its jwks_url document. Only EC P-256 (kty EC, crv P-256)
// and Ed25519 (kty OKP, crv Ed25519) public keys are understood.
type JWK struct {
	Kid string `json:"kid"`
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// Default JWKS cache timings. The unknown-kid refetch floor is fixed: an
// unauthenticated caller choosing a kid must never be able to make fuse hammer
// the issuer's key endpoint.
const (
	DefaultJWKSRefresh     = 5 * time.Minute
	jwksUnknownKidInterval = time.Minute
	jwksFetchTimeout       = 10 * time.Second
	maxJWKSBytes           = 1 << 20
)

// issuerKey is a parsed public key tagged with the JWS alg it may verify.
type issuerKey struct {
	kid string
	alg string // ES256 or EdDSA
	ec  *ecdsa.PublicKey
	ed  ed25519.PublicKey
}

var tenantIDRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// ValidTenantID reports whether t is a well-formed tenant id for prefix
// matching: lowercase letters, digits, "-" and "_", starting alphanumeric.
func ValidTenantID(t string) bool { return tenantIDRE.MatchString(t) }

func b64url(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }

// ParseJWK validates and converts a public JWK.
func ParseJWK(j JWK) (issuerKey, error) {
	if j.Kid == "" {
		return issuerKey{}, errors.New("jwk has no kid")
	}
	switch {
	case j.Kty == "EC" && j.Crv == "P-256":
		x, err := b64url(j.X)
		if err != nil || len(x) != 32 {
			return issuerKey{}, fmt.Errorf("jwk %q: bad x", j.Kid)
		}
		y, err := b64url(j.Y)
		if err != nil || len(y) != 32 {
			return issuerKey{}, fmt.Errorf("jwk %q: bad y", j.Kid)
		}
		raw := append(append([]byte{4}, x...), y...)
		if _, err := ecdh.P256().NewPublicKey(raw); err != nil {
			return issuerKey{}, fmt.Errorf("jwk %q: point is not on P-256", j.Kid)
		}
		pub := &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
		return issuerKey{kid: j.Kid, alg: "ES256", ec: pub}, nil
	case j.Kty == "OKP" && j.Crv == "Ed25519":
		x, err := b64url(j.X)
		if err != nil || len(x) != ed25519.PublicKeySize {
			return issuerKey{}, fmt.Errorf("jwk %q: bad x", j.Kid)
		}
		return issuerKey{kid: j.Kid, alg: "EdDSA", ed: ed25519.PublicKey(x)}, nil
	}
	return issuerKey{}, fmt.Errorf("jwk %q: unsupported kty/crv %q/%q (want EC/P-256 or OKP/Ed25519)", j.Kid, j.Kty, j.Crv)
}

func parseJWKs(jwks []JWK) (map[string]issuerKey, error) {
	keys := make(map[string]issuerKey, len(jwks))
	for _, j := range jwks {
		k, err := ParseJWK(j)
		if err != nil {
			return nil, err
		}
		if _, dup := keys[k.kid]; dup {
			return nil, fmt.Errorf("duplicate kid %q", k.kid)
		}
		keys[k.kid] = k
	}
	return keys, nil
}

// verifySig checks a JWS signature over input with this key.
func (k issuerKey) verifySig(input string, hash []byte, sig []byte) bool {
	switch k.alg {
	case "ES256":
		if len(sig) != 64 {
			return false
		}
		return ecdsa.Verify(k.ec, hash, new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:]))
	case "EdDSA":
		return ed25519.Verify(k.ed, []byte(input), sig)
	}
	return false
}

// keySource resolves a kid to a key for one issuer.
type keySource interface {
	lookup(ctx context.Context, kid string) (issuerKey, bool)
	kids() []string
}

type staticKeys map[string]issuerKey

func (s staticKeys) lookup(_ context.Context, kid string) (issuerKey, bool) {
	k, ok := s[kid]
	return k, ok
}
func (s staticKeys) kids() []string { return sortedKids(s) }

func sortedKids(m map[string]issuerKey) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// urlKeys is a cached JWKS document. It refetches every refresh interval and,
// on an unknown kid, at most once per jwksUnknownKidInterval. A failed refetch
// keeps serving the last good document.
type urlKeys struct {
	url     string
	refresh time.Duration
	client  *http.Client
	now     func() time.Time

	mu          sync.Mutex
	keys        map[string]issuerKey
	fetchedAt   time.Time
	unknownLast time.Time
	lastAttempt time.Time
	inflight    bool
}

func newURLKeys(url string, refresh time.Duration, client *http.Client, now func() time.Time) *urlKeys {
	if refresh <= 0 {
		refresh = DefaultJWKSRefresh
	}
	if client == nil {
		client = &http.Client{Timeout: jwksFetchTimeout, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse // a redirect could downgrade https to http
		}}
	}
	return &urlKeys{url: url, refresh: refresh, client: client, now: now}
}

// fetch downloads and parses the document without holding the lock, then swaps
// the key set in. It runs on a detached context so a caller's short deadline or
// disconnect cannot be used to make refreshes fail.
func (u *urlKeys) fetch() error {
	ctx, cancel := context.WithTimeout(context.Background(), jwksFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.url, nil)
	if err != nil {
		return err
	}
	resp, err := u.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("jwks fetch: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJWKSBytes))
	if err != nil {
		return err
	}
	var doc struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return fmt.Errorf("jwks fetch: %w", err)
	}
	// Keys this verifier cannot use (RSA, a different curve) are skipped rather
	// than failing the whole document: a JWKS commonly mixes key types.
	keys := map[string]issuerKey{}
	for _, raw := range doc.Keys {
		var j JWK
		if json.Unmarshal(raw, &j) != nil {
			continue
		}
		if k, err := ParseJWK(j); err == nil {
			keys[k.kid] = k
		}
	}
	if len(keys) == 0 {
		return errors.New("jwks fetch: document has no usable keys")
	}
	u.mu.Lock()
	u.keys = keys
	u.fetchedAt = u.now()
	u.mu.Unlock()
	return nil
}

// attempt runs one fetch, recording the attempt time whether or not it
// succeeds. Callers hold no lock.
func (u *urlKeys) attempt() {
	u.mu.Lock()
	u.lastAttempt = u.now()
	u.mu.Unlock()
	_ = u.fetch() // failure keeps the cached document
}

func (u *urlKeys) lookup(_ context.Context, kid string) (issuerKey, bool) {
	u.mu.Lock()
	now := u.now()
	if u.inflight {
		k, ok := u.keys[kid]
		u.mu.Unlock()
		return k, ok // stale-while-revalidate: use the cache, never queue
	}
	// A refresh is due when the document is older than refresh AND the last
	// attempt (even a failed one) is older than refresh.
	stale := u.keys == nil || (now.Sub(u.fetchedAt) >= u.refresh && now.Sub(u.lastAttempt) >= u.refresh)
	_, known := u.keys[kid]
	unknown := !known && now.Sub(u.unknownLast) >= jwksUnknownKidInterval
	if !stale && !unknown {
		k, ok := u.keys[kid]
		u.mu.Unlock()
		return k, ok
	}
	if unknown {
		u.unknownLast = now
	}
	u.inflight = true
	u.mu.Unlock()

	u.attempt()

	u.mu.Lock()
	u.inflight = false
	k, ok := u.keys[kid]
	u.mu.Unlock()
	return k, ok
}

func (u *urlKeys) kids() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return sortedKids(u.keys)
}
