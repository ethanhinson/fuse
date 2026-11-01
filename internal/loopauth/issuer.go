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
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/ethanhinson/fuse/internal/event"
)

// Issuer is one trusted token issuer: a separate service (for example a CMS that
// signs its own users in) that either shares an HS256 secret with fuse or publishes asymmetric keys
// (inline jwks or a jwks_url), and mints
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
	// Key is the HS256 shared secret. Exactly one of Key, JWKS and JWKSURL is set.
	Key []byte
	// JWKS is an inline list of public keys (EC P-256 or Ed25519); tokens are
	// ES256 or EdDSA and select their key by the JOSE "kid" header.
	JWKS []JWK
	// JWKSURL is an https URL of a JWKS document, fetched at construction and
	// cached (see JWKSRefresh); a fetch failure at construction is an error.
	JWKSURL string
	// JWKSRefresh is the JWKS cache lifetime; zero means DefaultJWKSRefresh.
	JWKSRefresh time.Duration
	// HTTPClient fetches JWKSURL; nil means a client with a 10s timeout.
	HTTPClient *http.Client
	// TenantPrefix accepts any valid tenant id starting with it, in addition
	// to Tenants.
	TenantPrefix string
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

// IssuerVerifier verifies compact JWS bearer tokens against a fixed set of
// trusted issuers. Its issuer set is fixed at construction; JWKS-backed issuers
// refresh their key caches internally. It is safe for concurrent use.
//
// Beyond the required claims, a token may carry the optional "scope",
// "harness_digest", "azp" and "jti" claims; see Verify.
type IssuerVerifier struct {
	issuers map[string]issuerEntry
	now     func() time.Time
	nowFn   func() time.Time // late-bound view of now, for JWKS caches
}

func oneKeySource(is Issuer) bool {
	n := 0
	if len(is.Key) > 0 {
		n++
	}
	if len(is.JWKS) > 0 {
		n++
	}
	if is.JWKSURL != "" {
		n++
	}
	return n == 1
}

type issuerEntry struct {
	Issuer
	tenants map[event.TenantID]bool
	keys    keySource // nil for a shared-secret (HS256) issuer
}

// KeySummary names one issuer's loaded verification keys, for the startup log.
type KeySummary struct {
	Issuer string
	Kids   []string
}

// KeySummaries lists the key ids loaded for each JWK issuer, ordered by name.
func (v *IssuerVerifier) KeySummaries() []KeySummary {
	var out []KeySummary
	for name, e := range v.issuers {
		if e.keys != nil {
			out = append(out, KeySummary{Issuer: name, Kids: e.keys.kids()})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Issuer < out[j].Issuer })
	return out
}

// NewIssuerVerifier builds an IssuerVerifier. It refuses an empty name, an
// empty key, an empty audience, a non-positive MaxTTL and a duplicate name, so a
// verifier that exists is one whose every issuer can be checked fully. Config
// validation reports the same problems earlier with the config key names; this
// is the last line for callers that build issuers in code.
func NewIssuerVerifier(issuers []Issuer) (*IssuerVerifier, error) {
	v := &IssuerVerifier{issuers: make(map[string]issuerEntry, len(issuers)), now: time.Now}
	v.nowFn = func() time.Time { return v.now() }
	for i, is := range issuers {
		switch {
		case is.Name == "":
			return nil, fmt.Errorf("loopauth: issuer %d has an empty name", i)
		case !oneKeySource(is):
			return nil, fmt.Errorf("loopauth: issuer %q needs exactly one of a signing key, jwks or jwks_url", is.Name)
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
		var ks keySource
		switch {
		case len(is.JWKS) > 0:
			parsed, err := parseJWKs(is.JWKS)
			if err != nil {
				return nil, fmt.Errorf("loopauth: issuer %q: %w", is.Name, err)
			}
			ks = staticKeys(parsed)
		case is.JWKSURL != "":
			uk := newURLKeys(is.JWKSURL, is.JWKSRefresh, is.HTTPClient, v.nowFn)
			if err := uk.fetch(); err != nil {
				return nil, fmt.Errorf("loopauth: issuer %q: initial jwks fetch failed: %w", is.Name, err)
			}
			ks = uk
		}
		key := make([]byte, len(is.Key))
		copy(key, is.Key)
		is.Key = key
		v.issuers[is.Name] = issuerEntry{Issuer: is, tenants: tenants, keys: ks}
	}
	return v, nil
}

// Verify resolves an issuer token to its Principal. Four optional claims are
// read onto it: "scope" (space-separated; absent means unrestricted, present but
// empty is refused) becomes Scopes, "harness_digest" becomes HarnessDigest,
// "azp" becomes ClientID and "jti" becomes TokenID; each must be a JSON string
// when present. Issuer is always the verifying issuer's trusted Name.
//
// Every failure, whatever the
// reason, is ErrInvalidToken: the Connect edge puts the error text on the wire,
// and a caller holding a bad token must not learn whether the issuer exists, the
// signature matched, or which claim was wrong.
func (v *IssuerVerifier) Verify(ctx context.Context, token string) (Principal, error) {
	p, err := v.verifyCtx(ctx, token)
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
	Kid  *string         `json:"kid"`
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
	// Scope is the optional RFC 8693 space-separated scope list. Absent means
	// unrestricted; present but null, or empty after splitting, is refused.
	// Kept raw so a present null is told apart from absence.
	Scope json.RawMessage `json:"scope"`
	// HarnessDigest optionally pins the token to one harness spec digest.
	// Absent means unpinned; present but null or empty is refused. Kept raw for
	// the same reason as Scope.
	HarnessDigest json.RawMessage `json:"harness_digest"`
	// Azp is the optional authorized party (OAuth client id), audit only.
	Azp string `json:"azp"`
	// Jti is the optional token id, audit only.
	Jti string `json:"jti"`
}

// verify is Verify with the reason kept, for tests. Order matters: nothing in
// the payload is trusted until the issuer named by the UNVERIFIED "iss" has
// been looked up and that issuer's key has verified the signature. Only "iss"
// is read before that, and only to pick the key.
func (v *IssuerVerifier) verify(token string) (Principal, error) {
	return v.verifyCtx(context.Background(), token)
}

func (v *IssuerVerifier) verifyCtx(ctx context.Context, token string) (Principal, error) {
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
	// Only HS256, ES256 and EdDSA exist here. "none", RS256, a missing alg and
	// case variants are refused before any key is touched; which of the three is
	// acceptable depends on the issuer's key type, checked below.
	if hdr.Alg == nil || (*hdr.Alg != "HS256" && *hdr.Alg != "ES256" && *hdr.Alg != "EdDSA") {
		return Principal{}, errors.New("alg is not supported")
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
	signingInput := parts[0] + "." + parts[1]
	if is.keys == nil {
		// Shared-secret issuer: HS256 only, and an asymmetric alg is never
		// reinterpreted against the secret.
		if *hdr.Alg != "HS256" {
			return Principal{}, errors.New("alg does not match key type")
		}
		mac := hmac.New(sha256.New, is.Key)
		mac.Write([]byte(signingInput))
		if subtle.ConstantTimeCompare(mac.Sum(nil), sig) != 1 {
			return Principal{}, errors.New("signature mismatch")
		}
	} else {
		// A key-set issuer never takes HS256; refuse it before the kid lookup
		// so an unauthenticated token cannot spend the unknown-kid refetch budget.
		if *hdr.Alg == "HS256" {
			return Principal{}, errors.New("alg does not match key type")
		}
		if hdr.Kid == nil || *hdr.Kid == "" {
			return Principal{}, errors.New("kid missing")
		}
		key, ok := is.keys.lookup(ctx, *hdr.Kid)
		if !ok {
			return Principal{}, errors.New("unknown kid")
		}
		if key.alg != *hdr.Alg {
			return Principal{}, errors.New("alg does not match key type")
		}
		sum := sha256.Sum256([]byte(signingInput))
		if !key.verifySig(signingInput, sum[:], sig) {
			return Principal{}, errors.New("signature mismatch")
		}
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
	if !is.tenants[tenant] && !(is.TenantPrefix != "" && strings.HasPrefix(string(tenant), is.TenantPrefix) && ValidTenantID(string(tenant))) {
		return Principal{}, fmt.Errorf("issuer may not assert tenant %q", tenant)
	}

	// A present scope claim that names nothing (null, or empty after
	// splitting) is refused: read as "no scope", "the issuer allowed nothing"
	// would silently become "everything allowed".
	var scopes Scopes
	if c.Scope != nil {
		raw, err := presentStringClaim(c.Scope, "scope")
		if err != nil {
			return Principal{}, err
		}
		scopes = ParseScopes(raw)
		if scopes.Empty() {
			return Principal{}, errors.New("scope claim is present but empty")
		}
	}
	// Likewise a present harness_digest that pins nothing is refused rather
	// than read as unpinned.
	var digest string
	if c.HarnessDigest != nil {
		d, err := presentStringClaim(c.HarnessDigest, "harness_digest")
		if err != nil {
			return Principal{}, err
		}
		if d == "" {
			return Principal{}, errors.New("harness_digest claim is present but empty")
		}
		digest = d
	}

	return Principal{
		Tenant:                tenant,
		Subject:               c.Sub,
		ObservabilityOperator: is.ObservabilityOperator,
		Scopes:                scopes,
		HarnessDigest:         digest,
		ClientID:              c.Azp,
		Issuer:                is.Name,
		TokenID:               c.Jti,
	}, nil
}

// presentStringClaim decodes an optional claim known to be present. A literal
// null is refused (it is present, so it cannot mean "absent"), and a non-string
// value is malformed like any other mistyped claim.
func presentStringClaim(raw json.RawMessage, name string) (string, error) {
	if string(raw) == "null" {
		return "", fmt.Errorf("%s claim is present but null", name)
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", errors.New("malformed claims")
	}
	return v, nil
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
