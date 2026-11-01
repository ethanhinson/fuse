package loopauth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/event"
)

var (
	testNow    = time.Unix(1_800_000_000, 0)
	testKey    = []byte("0123456789abcdef0123456789abcdef-cms")
	otherKey   = []byte("fedcba9876543210fedcba9876543210-evil")
	testIssuer = Issuer{
		Name:     "cms",
		Key:      testKey,
		Audience: "fuse-loop",
		Tenants:  []event.TenantID{event.DefaultTenant, "acme"},
		MaxTTL:   time.Hour,
	}
)

func newTestIssuerVerifier(t *testing.T, issuers ...Issuer) *IssuerVerifier {
	t.Helper()
	if len(issuers) == 0 {
		issuers = []Issuer{testIssuer}
	}
	v, err := NewIssuerVerifier(issuers)
	if err != nil {
		t.Fatalf("NewIssuerVerifier: %v", err)
	}
	v.now = func() time.Time { return testNow }
	return v
}

func b64(v any) string {
	var raw []byte
	switch x := v.(type) {
	case string:
		raw = []byte(x)
	default:
		var err error
		raw, err = json.Marshal(x)
		if err != nil {
			panic(err)
		}
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// mint signs header+claims with key using HMAC-SHA256, regardless of what the
// header's alg says, so alg-confusion cases carry a signature that WOULD verify.
func mint(header, claims any, key []byte) string {
	input := b64(header) + "." + b64(claims)
	m := hmac.New(sha256.New, key)
	m.Write([]byte(input))
	return input + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func hs256() map[string]any { return map[string]any{"alg": "HS256", "typ": "JWT"} }

func goodClaims() map[string]any {
	return map[string]any{
		"iss": "cms",
		"sub": "user-42",
		"aud": "fuse-loop",
		"iat": testNow.Add(-time.Minute).Unix(),
		"exp": testNow.Add(10 * time.Minute).Unix(),
	}
}

func with(c map[string]any, kv ...any) map[string]any {
	out := map[string]any{}
	for k, v := range c {
		out[k] = v
	}
	for i := 0; i < len(kv); i += 2 {
		k := kv[i].(string)
		if kv[i+1] == nil {
			delete(out, k)
			continue
		}
		out[k] = kv[i+1]
	}
	return out
}

func TestIssuerTokenAccepted(t *testing.T) {
	v := newTestIssuerVerifier(t)
	cases := []struct {
		name   string
		claims map[string]any
		want   Principal
	}{
		{"tenant omitted is _default", goodClaims(), Principal{Tenant: event.DefaultTenant, Subject: "user-42", Issuer: "cms"}},
		{"allowed named tenant", with(goodClaims(), "tenant", "acme"), Principal{Tenant: "acme", Subject: "user-42", Issuer: "cms"}},
		{"empty tenant is _default", with(goodClaims(), "tenant", ""), Principal{Tenant: event.DefaultTenant, Subject: "user-42", Issuer: "cms"}},
		{"aud as array", with(goodClaims(), "aud", []string{"other", "fuse-loop"}), Principal{Tenant: event.DefaultTenant, Subject: "user-42", Issuer: "cms"}},
		{"iat slightly ahead within skew", with(goodClaims(), "iat", testNow.Add(IssuerClockSkew-time.Second).Unix()), Principal{Tenant: event.DefaultTenant, Subject: "user-42", Issuer: "cms"}},
		{"lifetime exactly max_ttl", with(goodClaims(), "iat", testNow.Add(-time.Minute).Unix(), "exp", testNow.Add(59*time.Minute).Unix()), Principal{Tenant: event.DefaultTenant, Subject: "user-42", Issuer: "cms"}},
		{"fractional NumericDate", with(goodClaims(), "exp", float64(testNow.Unix())+30.5), Principal{Tenant: event.DefaultTenant, Subject: "user-42", Issuer: "cms"}},
		{"nbf in the past", with(goodClaims(), "nbf", testNow.Add(-time.Minute).Unix()), Principal{Tenant: event.DefaultTenant, Subject: "user-42", Issuer: "cms"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := v.Verify(context.Background(), mint(hs256(), tc.claims, testKey))
			if err != nil {
				reason, _ := v.verify(mint(hs256(), tc.claims, testKey))
				t.Fatalf("Verify: %v (reason: %v)", err, reason)
			}
			if got != tc.want {
				t.Fatalf("principal = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestIssuerTokenRefused(t *testing.T) {
	v := newTestIssuerVerifier(t)
	good := goodClaims()
	cases := []struct {
		name   string
		token  string
		reason string // substring of the internal reason, to prove the RIGHT check refused it
	}{
		{"empty", "", "empty"},
		{"static-looking token", "s3cr3t-alice", "not a compact JWS"},
		{"two segments", b64(hs256()) + "." + b64(good), "not a compact JWS"},
		{"oversized", strings.Repeat("a", maxIssuerTokenBytes+1), "too large"},

		// Alg confusion: each of these carries an HMAC signature under the real
		// key, so only the alg check can be what refuses them.
		{"alg none", mint(map[string]any{"alg": "none"}, good, testKey), "alg"},
		{"alg none unsigned", b64(map[string]any{"alg": "none"}) + "." + b64(good) + ".", "alg"},
		{"alg RS256", mint(map[string]any{"alg": "RS256"}, good, testKey), "alg"},
		{"alg HS512", mint(map[string]any{"alg": "HS512"}, good, testKey), "alg"},
		{"alg lowercase", mint(map[string]any{"alg": "hs256"}, good, testKey), "alg"},
		{"alg missing", mint(map[string]any{"typ": "JWT"}, good, testKey), "alg"},
		{"alg not a string", mint(map[string]any{"alg": 256}, good, testKey), "malformed header"},
		{"crit header", mint(map[string]any{"alg": "HS256", "crit": []string{"exp"}}, good, testKey), "crit"},

		{"bad signature (other key)", mint(hs256(), good, otherKey), "signature"},
		{"tampered payload", func() string {
			tok := mint(hs256(), good, testKey)
			parts := strings.Split(tok, ".")
			return parts[0] + "." + b64(with(good, "sub", "admin")) + "." + parts[2]
		}(), "signature"},
		{"signature not base64url", mint(hs256(), good, testKey) + "!", "signature"},
		{"unknown issuer", mint(hs256(), with(good, "iss", "elsewhere"), testKey), "unknown issuer"},
		{"iss missing", mint(hs256(), with(good, "iss", nil), testKey), "unknown issuer"},

		{"sub missing", mint(hs256(), with(good, "sub", nil), testKey), "sub"},
		{"sub empty", mint(hs256(), with(good, "sub", ""), testKey), "sub"},
		{"sub not a string", mint(hs256(), with(good, "sub", 7), testKey), "malformed claims"},
		{"aud wrong", mint(hs256(), with(good, "aud", "someone-else"), testKey), "aud"},
		{"aud missing", mint(hs256(), with(good, "aud", nil), testKey), "aud"},
		{"aud array without ours", mint(hs256(), with(good, "aud", []string{"a", "b"}), testKey), "aud"},

		{"exp missing", mint(hs256(), with(good, "exp", nil), testKey), "exp missing"},
		{"iat missing", mint(hs256(), with(good, "iat", nil), testKey), "iat missing"},
		{"exp not a number", mint(hs256(), with(good, "exp", "soon"), testKey), "malformed claims"},
		{"expired", mint(hs256(), with(good, "iat", testNow.Add(-20*time.Minute).Unix(), "exp", testNow.Add(-time.Second).Unix()), testKey), "expired"},
		{"expires exactly now", mint(hs256(), with(good, "exp", testNow.Unix()), testKey), "expired"},
		{"iat in the future beyond skew", mint(hs256(), with(good, "iat", testNow.Add(IssuerClockSkew+time.Second).Unix(), "exp", testNow.Add(10*time.Minute).Unix()), testKey), "future"},
		{"exp before iat", mint(hs256(), with(good, "iat", testNow.Add(20*time.Second).Unix(), "exp", testNow.Add(10*time.Second).Unix()), testKey), "not after iat"},
		{"lifetime over max_ttl", mint(hs256(), with(good, "iat", testNow.Add(-time.Minute).Unix(), "exp", testNow.Add(59*time.Minute+time.Second).Unix()), testKey), "max_ttl"},
		{"nbf in the future", mint(hs256(), with(good, "nbf", testNow.Add(time.Hour).Unix()), testKey), "nbf"},
		{"negative iat", mint(hs256(), with(good, "iat", -5), testKey), "NumericDate"},

		// Tenant escalation: the issuer may only assert its own tenants.
		{"tenant not allowed", mint(hs256(), with(good, "tenant", "globex"), testKey), "tenant"},
		{"tenant case variant", mint(hs256(), with(good, "tenant", "ACME"), testKey), "tenant"},
		{"tenant not a string", mint(hs256(), with(good, "tenant", []string{"acme"}), testKey), "malformed claims"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := v.Verify(context.Background(), tc.token)
			if !errors.Is(err, ErrInvalidToken) || err != ErrInvalidToken {
				t.Fatalf("Verify err = %v, want exactly ErrInvalidToken (no detail on the wire)", err)
			}
			if p != (Principal{}) {
				t.Fatalf("refused token still returned principal %+v", p)
			}
			_, reason := v.verify(tc.token)
			if reason == nil || !strings.Contains(reason.Error(), tc.reason) {
				t.Fatalf("refusal reason = %v, want it to mention %q", reason, tc.reason)
			}
		})
	}
}

// The optional scope, harness_digest, azp and jti claims land on the
// Principal, scopes canonicalized (deduplicated, sorted), and Issuer is the
// verifying issuer's trusted Name.
func TestIssuerTokenCarriesScopesAndAuditClaims(t *testing.T) {
	v := newTestIssuerVerifier(t)
	claims := with(goodClaims(), "scope", "loop:start loop:observe loop:start", "harness_digest", "sha256:abc", "azp", "client-1", "jti", "x")
	got, err := v.Verify(context.Background(), mint(hs256(), claims, testKey))
	if err != nil {
		_, reason := v.verify(mint(hs256(), claims, testKey))
		t.Fatalf("Verify: %v (reason: %v)", err, reason)
	}
	want := Principal{
		Tenant:        event.DefaultTenant,
		Subject:       "user-42",
		Scopes:        NewScopes("loop:observe", "loop:start"),
		HarnessDigest: "sha256:abc",
		ClientID:      "client-1",
		Issuer:        "cms",
		TokenID:       "x",
	}
	if got != want {
		t.Fatalf("principal = %+v, want %+v", got, want)
	}
}

// No scope claim means unrestricted (empty Scopes), and no digest pin.
func TestIssuerTokenWithoutScopeIsUnrestricted(t *testing.T) {
	v := newTestIssuerVerifier(t)
	got, err := v.Verify(context.Background(), mint(hs256(), goodClaims(), testKey))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.Scopes != "" || !got.Scopes.Empty() {
		t.Fatalf("Scopes = %q, want empty (unrestricted)", got.Scopes)
	}
	if got.HarnessDigest != "" {
		t.Fatalf("HarnessDigest = %q, want empty", got.HarnessDigest)
	}
	if got.Issuer != "cms" {
		t.Fatalf("Issuer = %q, want cms", got.Issuer)
	}
}

// jsonNull is a claim value that marshals to a literal JSON null (a nil value
// passed to with deletes the key instead, which tests absence).
var jsonNull = json.RawMessage("null")

// A present-but-empty or null scope claim, and a present null or empty
// harness_digest, is refused rather than read as unrestricted or unpinned, and
// a non-string scope, harness_digest, azp or jti fails claim decoding. Every
// refusal is the uniform ErrInvalidToken with a zero Principal.
func TestIssuerTokenRefusesBadScopeAndAuditClaims(t *testing.T) {
	v := newTestIssuerVerifier(t)
	good := goodClaims()
	cases := []struct {
		name   string
		claims map[string]any
		reason string
	}{
		{"scope empty", with(good, "scope", ""), "scope"},
		{"scope whitespace only", with(good, "scope", "   "), "scope"},
		{"scope array", with(good, "scope", []string{"loop:start"}), "malformed claims"},
		{"scope null", with(good, "scope", jsonNull), "scope"},
		{"harness_digest number", with(good, "harness_digest", 7), "malformed claims"},
		{"harness_digest null", with(good, "harness_digest", jsonNull), "harness_digest"},
		{"harness_digest empty", with(good, "harness_digest", ""), "harness_digest"},
		{"azp bool", with(good, "azp", true), "malformed claims"},
		{"jti object", with(good, "jti", map[string]any{}), "malformed claims"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tok := mint(hs256(), tc.claims, testKey)
			p, err := v.Verify(context.Background(), tok)
			if err != ErrInvalidToken {
				t.Fatalf("Verify err = %v, want exactly ErrInvalidToken", err)
			}
			if p != (Principal{}) {
				t.Fatalf("refused token still returned principal %+v", p)
			}
			_, reason := v.verify(tok)
			if reason == nil || !strings.Contains(reason.Error(), tc.reason) {
				t.Fatalf("refusal reason = %v, want it to mention %q", reason, tc.reason)
			}
		})
	}
}

// An issuer's token cannot claim the observability operator grant; only the
// issuer's trusted config entry can give it.
func TestIssuerObservabilityOperatorComesFromConfigOnly(t *testing.T) {
	v := newTestIssuerVerifier(t)
	p, err := v.Verify(context.Background(), mint(hs256(), with(goodClaims(), "observability_operator", true), testKey))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if p.ObservabilityOperator {
		t.Fatal("a token claim granted observability_operator; only issuer config may")
	}

	op := testIssuer
	op.ObservabilityOperator = true
	v = newTestIssuerVerifier(t, op)
	p, err = v.Verify(context.Background(), mint(hs256(), goodClaims(), testKey))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !p.ObservabilityOperator {
		t.Fatal("issuer configured with observability_operator did not grant it")
	}
}

// Two issuers with different keys: a token naming one issuer but signed with the
// other's key is refused, so one issuer can never mint for another.
func TestIssuerKeysAreNotInterchangeable(t *testing.T) {
	other := Issuer{Name: "billing", Key: otherKey, Audience: "fuse-loop", Tenants: []event.TenantID{"globex"}, MaxTTL: time.Hour}
	v := newTestIssuerVerifier(t, testIssuer, other)

	if _, err := v.Verify(context.Background(), mint(hs256(), with(goodClaims(), "iss", "billing", "tenant", "globex"), otherKey)); err != nil {
		t.Fatalf("billing token under billing key: %v", err)
	}
	if _, err := v.Verify(context.Background(), mint(hs256(), with(goodClaims(), "iss", "billing", "tenant", "globex"), testKey)); err == nil {
		t.Fatal("billing token signed with the cms key was accepted")
	}
	if _, err := v.Verify(context.Background(), mint(hs256(), with(goodClaims(), "tenant", "globex"), testKey)); err == nil {
		t.Fatal("cms asserted billing's tenant globex")
	}
}

func TestNewIssuerVerifierRefusesIncompleteIssuers(t *testing.T) {
	cases := map[string][]Issuer{
		"empty name":     {{Key: testKey, Audience: "a", MaxTTL: time.Hour}},
		"empty key":      {{Name: "cms", Audience: "a", MaxTTL: time.Hour}},
		"empty audience": {{Name: "cms", Key: testKey, MaxTTL: time.Hour}},
		"zero max ttl":   {{Name: "cms", Key: testKey, Audience: "a"}},
		"negative ttl":   {{Name: "cms", Key: testKey, Audience: "a", MaxTTL: -time.Second}},
		"duplicate":      {testIssuer, testIssuer},
	}
	for name, issuers := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := NewIssuerVerifier(issuers); err == nil {
				t.Fatal("NewIssuerVerifier accepted an incomplete issuer")
			}
		})
	}
}

// FirstOf tries the static map first, then issuers, and hides every failure
// behind ErrInvalidToken.
func TestFirstOfStaticThenIssuer(t *testing.T) {
	static := NewStaticVerifier(map[string]Principal{
		"static-tok": {Tenant: "acme", Subject: "alice", ObservabilityOperator: true},
	})
	v := FirstOf(static, newTestIssuerVerifier(t))

	p, err := v.Verify(context.Background(), "static-tok")
	if err != nil || p != (Principal{Tenant: "acme", Subject: "alice", ObservabilityOperator: true}) {
		t.Fatalf("static token: %+v, %v", p, err)
	}
	p, err = v.Verify(context.Background(), mint(hs256(), goodClaims(), testKey))
	if err != nil || p != (Principal{Tenant: event.DefaultTenant, Subject: "user-42", Issuer: "cms"}) {
		t.Fatalf("issuer token: %+v, %v", p, err)
	}
	for _, bad := range []string{"", "nope", mint(hs256(), goodClaims(), otherKey)} {
		if _, err := v.Verify(context.Background(), bad); err != ErrInvalidToken {
			t.Fatalf("Verify(%q) err = %v, want ErrInvalidToken", bad, err)
		}
	}
}
