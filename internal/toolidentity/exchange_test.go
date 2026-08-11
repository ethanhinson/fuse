package toolidentity

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/event"
)

// decodeClaims splits a compact JWS and returns the decoded payload claims. It
// does NOT verify the signature — signature verification is asserted separately.
func decodeClaims(t *testing.T, token string) map[string]any {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token is not a 3-part JWS: %q", token)
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode claims: %v", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatalf("unmarshal claims: %v", err)
	}
	return claims
}

func newTestSTS(t *testing.T) *BuiltinSTS {
	t.Helper()
	sts, err := NewBuiltinSTS(BuiltinSTSConfig{
		Issuer: "fuse",
		TTL:    5 * time.Minute,
		// Two tenants with distinct signing keys — isolation is asserted below.
		TenantKeys: map[event.TenantID][]byte{
			"tenantA": []byte("key-for-tenant-a-0123456789abcdef"),
			"tenantB": []byte("key-for-tenant-b-fedcba9876543210"),
		},
	})
	if err != nil {
		t.Fatalf("NewBuiltinSTS: %v", err)
	}
	return sts
}

// Two different subjects produce tokens whose `sub` differs and both carry the
// `act` delegation claim naming fuse — delegation, not impersonation.
func TestBuiltinSTS_DelegationPerSubject(t *testing.T) {
	sts := newTestSTS(t)
	ctx := context.Background()

	mint := func(subject string) map[string]any {
		res, err := sts.Exchange(ctx, ExchangeRequest{
			Tenant:   "tenantA",
			Subject:  subject,
			Actor:    "fuse",
			Audience: "https://api.example.com",
			Scopes:   []string{"read"},
		})
		if err != nil {
			t.Fatalf("Exchange(%s): %v", subject, err)
		}
		return decodeClaims(t, res.Token)
	}

	a := mint("alice")
	b := mint("bob")

	if a["sub"] != "alice" || b["sub"] != "bob" {
		t.Fatalf("sub not per-subject: a=%v b=%v", a["sub"], b["sub"])
	}
	if a["sub"] == b["sub"] {
		t.Fatal("two principals produced the same sub")
	}
	for who, claims := range map[string]map[string]any{"alice": a, "bob": b} {
		act, ok := claims["act"].(map[string]any)
		if !ok {
			t.Fatalf("%s: missing act claim (delegation): %v", who, claims["act"])
		}
		if act["sub"] != "fuse" {
			t.Fatalf("%s: act.sub = %v, want fuse", who, act["sub"])
		}
	}
}

// The minted token is audience-bound to the requested resource and echoes only
// the requested scope — no scope expansion.
func TestBuiltinSTS_AudienceAndScopeBinding(t *testing.T) {
	sts := newTestSTS(t)
	res, err := sts.Exchange(context.Background(), ExchangeRequest{
		Tenant:   "tenantA",
		Subject:  "alice",
		Actor:    "fuse",
		Audience: "https://target-a.example.com",
		Scopes:   []string{"read", "list"},
	})
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	claims := decodeClaims(t, res.Token)
	if claims["aud"] != "https://target-a.example.com" {
		t.Fatalf("aud = %v, want the requested audience", claims["aud"])
	}
	scope, _ := claims["scope"].(string)
	if scope != "read list" {
		t.Fatalf("scope = %q, want %q", scope, "read list")
	}
	if strings.Contains(scope, "admin") || strings.Contains(scope, "write") {
		t.Fatalf("scope expanded beyond request: %q", scope)
	}
}

// The token is short-lived: exp is within the configured TTL of iat.
func TestBuiltinSTS_ShortLived(t *testing.T) {
	sts := newTestSTS(t)
	res, err := sts.Exchange(context.Background(), ExchangeRequest{
		Tenant:   "tenantA",
		Subject:  "alice",
		Actor:    "fuse",
		Audience: "https://api.example.com",
	})
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if res.Expiry.IsZero() {
		t.Fatal("ExchangeResult.Expiry not set")
	}
	if d := time.Until(res.Expiry); d <= 0 || d > 6*time.Minute {
		t.Fatalf("token TTL out of range: %v", d)
	}
	claims := decodeClaims(t, res.Token)
	iat, _ := claims["iat"].(float64)
	exp, _ := claims["exp"].(float64)
	if exp <= iat {
		t.Fatalf("exp (%v) must be after iat (%v)", exp, iat)
	}
	if exp-iat > 6*60 {
		t.Fatalf("token lifetime %v seconds exceeds TTL", exp-iat)
	}
}

// A token minted under tenant A's key does not verify under tenant B's key —
// per-tenant signer isolation (a tenant's credential material is unreachable by
// another tenant).
func TestBuiltinSTS_PerTenantSignerIsolation(t *testing.T) {
	sts := newTestSTS(t)
	ctx := context.Background()

	resA, err := sts.Exchange(ctx, ExchangeRequest{
		Tenant: "tenantA", Subject: "alice", Actor: "fuse", Audience: "https://api",
	})
	if err != nil {
		t.Fatalf("Exchange A: %v", err)
	}

	// Verifies under tenant A's key.
	if err := sts.Verify("tenantA", resA.Token); err != nil {
		t.Fatalf("token should verify under its own tenant key: %v", err)
	}
	// Must NOT verify under tenant B's key.
	if err := sts.Verify("tenantB", resA.Token); err == nil {
		t.Fatal("token minted for tenant A must not verify under tenant B's key")
	}
}

// A tenant with no configured key cannot mint — fail closed, never with an empty
// or shared key.
func TestBuiltinSTS_UnknownTenantFails(t *testing.T) {
	sts := newTestSTS(t)
	_, err := sts.Exchange(context.Background(), ExchangeRequest{
		Tenant: "no-such-tenant", Subject: "alice", Actor: "fuse", Audience: "https://api",
	})
	if err == nil {
		t.Fatal("Exchange for an unconfigured tenant must fail")
	}
}
