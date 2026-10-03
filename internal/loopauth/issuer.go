package loopauth

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/ethanhinson/fuse/internal/event"
)

// Issuer is one trusted token issuer: a separate service (for example a CMS that
// signs its own users in) that holds a shared HS256 secret with fuse and mints
// short-lived bearer tokens naming the subject per request. It is the mirror
// image of the delegation tokens fuse itself mints toward MCP servers
// (internal/toolidentity): there fuse signs and a downstream verifies, here a
// trusted upstream signs and fuse verifies.
//
// Every field comes from the trusted home config (loop_server.issuers). An
// issuer can only ever produce a Principal inside the limits written here: a
// tenant it is not allowed to assert is refused, and the observability operator
// grant comes from this struct, never from a token claim.
type Issuer struct {
	// Name must equal the token's "iss" claim. It is also how a token selects
	// which issuer verifies it.
	Name string
	// Key is the HS256 shared secret.
	Key []byte
	// Audience must equal the token's "aud" claim (or be one of its entries when
	// "aud" is an array).
	Audience string
	// Tenants are the tenants this issuer may assert. A token whose "tenant"
	// claim (omitted means event.DefaultTenant) is not in this list is refused.
	// Entries are compared after event.NormalizeTenant.
	Tenants []event.TenantID
	// MaxTTL bounds exp - iat. A token minted to live longer is refused, so a
	// leaked issuer token is only ever useful for a short window.
	MaxTTL time.Duration
	// ObservabilityOperator is copied onto every Principal this issuer produces.
	// It is false unless the operator set it on the issuer in trusted config.
	ObservabilityOperator bool
}

// IssuerClockSkew is how far in the future a token's "iat" (and "nbf") may be
// and still be accepted, to absorb small clock drift between the issuing
// service and fuse. Expiry gets no such allowance: an expired token is refused.
const IssuerClockSkew = 30 * time.Second

// maxIssuerTokenBytes caps how much of an unauthenticated bearer value is ever
// base64-decoded and JSON-parsed before the signature is checked. Real tokens
// are a few hundred bytes.
const maxIssuerTokenBytes = 8 << 10

// IssuerVerifier verifies compact HS256 JWS bearer tokens against a fixed set of
// trusted issuers. It is read-only after construction and safe for concurrent use.
type IssuerVerifier struct {
	issuers map[string]issuerEntry
	now     func() time.Time
}

type issuerEntry struct {
	Issuer
	tenants map[event.TenantID]bool
}

// NewIssuerVerifier builds an IssuerVerifier. It refuses an empty name, an
// empty key, an empty audience, a non-positive MaxTTL and a duplicate name, so a
// verifier that exists is one whose every issuer can be checked fully. Config
// validation reports the same problems earlier with the config key names; this
// is the last line for callers that build issuers in code.
func NewIssuerVerifier(issuers []Issuer) (*IssuerVerifier, error) {
	v := &IssuerVerifier{issuers: make(map[string]issuerEntry, len(issuers)), now: time.Now}
	for i, is := range issuers {
		switch {
		case is.Name == "":
			return nil, fmt.Errorf("loopauth: issuer %d has an empty name", i)
		case len(is.Key) == 0:
			return nil, fmt.Errorf("loopauth: issuer %q has an empty signing key", is.Name)
		case is.Audience == "":
			return nil, fmt.Errorf("loopauth: issuer %q has an empty audience", is.Name)
		case is.MaxTTL <= 0:
			return nil, fmt.Errorf("loopauth: issuer %q max TTL must be positive, got %s", is.Name, is.MaxTTL)
		}
		if _, dup := v.issuers[is.Name]; dup {
			return nil, fmt.Errorf("loopauth: issuer %q is declared twice", is.Name)
		}
		tenants := make(map[event.TenantID]bool, len(is.Tenants))
		for _, t := range is.Tenants {
			tenants[event.NormalizeTenant(t)] = true
		}
		key := make([]byte, len(is.Key))
		copy(key, is.Key)
		is.Key = key
		v.issuers[is.Name] = issuerEntry{Issuer: is, tenants: tenants}
	}
	return v, nil
}

// Verify resolves an issuer token to its Principal. Every failure, whatever the
// reason, is ErrInvalidToken: the Connect edge puts the error text on the wire,
// and a caller holding a bad token must not learn whether the issuer exists, the
// signature matched, or which claim was wrong.
func (v *IssuerVerifier) Verify(_ context.Context, token string) (Principal, error) {
	p, err := v.verify(token)
	if err != nil {
		return Principal{}, ErrInvalidToken
	}
	return p, nil
}

// issuerHeader is the JOSE header fields this verifier reads. Crit is decoded
// only to refuse it: no critical extension is understood here, and RFC 7515
// section 4.1.11 requires refusing a token that names one.
type issuerHeader struct {
	Alg  *string         `json:"alg"`
	Crit json.RawMessage `json:"crit"`
}

type issuerClaims struct {
	Iss    string          `json:"iss"`
	Sub    string          `json:"sub"`
	Aud    json.RawMessage `json:"aud"`
	Iat    *json.Number    `json:"iat"`
	Exp    *json.Number    `json:"exp"`
	Nbf    *json.Number    `json:"nbf"`
	Tenant *string         `json:"tenant"`
}

// verify is Verify with the reason kept, for tests. Order matters: nothing in
// the payload is trusted until the issuer named by the UNVERIFIED "iss" has
// been looked up and that issuer's key has verified the signature. Only "iss"
// is read before that, and only to pick the key.
func (v *IssuerVerifier) verify(token string) (Principal, error) {
	if token == "" || len(token) > maxIssuerTokenBytes {
		return Principal{}, errors.New("token empty or too large")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Principal{}, errors.New("not a compact JWS")
	}

	hdrBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return Principal{}, errors.New("malformed header encoding")
	}
	var hdr issuerHeader
	if err := json.Unmarshal(hdrBytes, &hdr); err != nil {
		return Principal{}, errors.New("malformed header")
	}
	// Exactly HS256. "none", RS256/ES256 (whose "public key" an attacker could
	// try to pass off as the HMAC secret), a missing alg and case variants are
	// all refused before any key is touched.
	if hdr.Alg == nil || *hdr.Alg != "HS256" {
		return Principal{}, errors.New("alg is not HS256")
	}
	if len(hdr.Crit) > 0 {
		return Principal{}, errors.New("crit header is not supported")
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Principal{}, errors.New("malformed payload encoding")
	}
	var unverified struct {
		Iss string `json:"iss"`
	}
	if err := json.Unmarshal(payload, &unverified); err != nil {
		return Principal{}, errors.New("malformed payload")
	}
	is, ok := v.issuers[unverified.Iss]
	if !ok {
		return Principal{}, fmt.Errorf("unknown issuer %q", unverified.Iss)
	}

	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Principal{}, errors.New("malformed signature encoding")
	}
	mac := hmac.New(sha256.New, is.Key)
	mac.Write([]byte(parts[0] + "." + parts[1]))
	if subtle.ConstantTimeCompare(mac.Sum(nil), sig) != 1 {
		return Principal{}, errors.New("signature mismatch")
	}

	// The signature holds; from here the payload is the issuer's word.
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	var c issuerClaims
	if err := dec.Decode(&c); err != nil {
		return Principal{}, errors.New("malformed claims")
	}
	if c.Iss != is.Name {
		return Principal{}, errors.New("iss mismatch")
	}
	if c.Sub == "" {
		return Principal{}, errors.New("sub missing")
	}
	if !audienceContains(c.Aud, is.Audience) {
		return Principal{}, errors.New("aud mismatch")
	}

	now := v.now()
	iat, err := numericDate(c.Iat, "iat")
	if err != nil {
		return Principal{}, err
	}
	exp, err := numericDate(c.Exp, "exp")
	if err != nil {
		return Principal{}, err
	}
	if iat.After(now.Add(IssuerClockSkew)) {
		return Principal{}, errors.New("iat is in the future")
	}
	if !now.Before(exp) {
		return Principal{}, errors.New("token expired")
	}
	if !exp.After(iat) {
		return Principal{}, errors.New("exp is not after iat")
	}
	if exp.Sub(iat) > is.MaxTTL {
		return Principal{}, fmt.Errorf("lifetime %s exceeds max_ttl %s", exp.Sub(iat), is.MaxTTL)
	}
	if c.Nbf != nil {
		nbf, err := numericDate(c.Nbf, "nbf")
		if err != nil {
			return Principal{}, err
		}
		if nbf.After(now.Add(IssuerClockSkew)) {
			return Principal{}, errors.New("token not yet valid (nbf)")
		}
	}

	tenant := event.DefaultTenant
	if c.Tenant != nil {
		tenant = event.NormalizeTenant(event.TenantID(*c.Tenant))
	}
	if !is.tenants[tenant] {
		return Principal{}, fmt.Errorf("issuer may not assert tenant %q", tenant)
	}

	return Principal{
		Tenant:                tenant,
		Subject:               c.Sub,
		ObservabilityOperator: is.ObservabilityOperator,
	}, nil
}

// audienceContains reports whether a JWT "aud" claim names want. Per RFC 7519
// section 4.1.3 "aud" is either one string or an array of strings.
func audienceContains(raw json.RawMessage, want string) bool {
	if len(raw) == 0 {
		return false
	}
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		return one == want
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err != nil {
		return false
	}
	for _, a := range many {
		if a == want {
			return true
		}
	}
	return false
}

// numericDate parses a required JWT NumericDate (seconds since the epoch, which
// RFC 7519 allows to be fractional).
func numericDate(n *json.Number, name string) (time.Time, error) {
	if n == nil {
		return time.Time{}, fmt.Errorf("%s missing", name)
	}
	f, err := n.Float64()
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 || f > 1<<40 {
		return time.Time{}, fmt.Errorf("%s is not a valid NumericDate", name)
	}
	sec, frac := math.Modf(f)
	return time.Unix(int64(sec), int64(frac*1e9)), nil
}

// FirstOf returns a Verifier that asks each verifier in order and returns the
// first Principal any of them resolves. If none does, the result is
// ErrInvalidToken. The loop server uses it to try the static token map before
// the trusted issuers, so an existing static token keeps resolving exactly as it
// did before issuers were configured.
func FirstOf(verifiers ...Verifier) Verifier {
	return firstOf(verifiers)
}

type firstOf []Verifier

func (f firstOf) Verify(ctx context.Context, token string) (Principal, error) {
	for _, v := range f {
		if p, err := v.Verify(ctx, token); err == nil {
			return p, nil
		}
	}
	return Principal{}, ErrInvalidToken
}
