package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/loopauth"
	loopv1 "github.com/ethanhinson/fuse/internal/loopwire/v1"
)

const issuerTestKey = "cms-shared-secret-at-least-32-bytes-long"

// issuerTestConfig is the loop_server block a CMS-style deployment writes: one
// static token kept from before, plus one trusted issuer for per-user tokens.
func issuerTestConfig() config.Config {
	return config.Config{LoopServer: config.LoopServerConfig{
		Auth: []config.AuthTokenConfig{{Token: "static-tok", Tenant: "acme", Subject: "ops"}},
		Issuers: []config.IssuerConfig{{
			Issuer:     "cms",
			SigningKey: issuerTestKey,
			Audience:   "fuse-loop",
			Tenants:    []string{"acme"},
			MaxTTL:     "15m",
		}},
	}}
}

// mintIssuerToken mints a token the way an integrating service would: a compact
// HS256 JWS over {iss, sub, aud, iat, exp, tenant}. It mirrors the TypeScript
// example in README.md.
func mintIssuerToken(t *testing.T, key string, claims map[string]any) string {
	t.Helper()
	enc := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	input := enc(map[string]string{"alg": "HS256", "typ": "JWT"}) + "." + enc(claims)
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte(input))
	return input + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func issuerClaims(sub, tenant string) map[string]any {
	now := time.Now()
	c := map[string]any{
		"iss": "cms",
		"sub": sub,
		"aud": "fuse-loop",
		"iat": now.Unix(),
		"exp": now.Add(5 * time.Minute).Unix(),
	}
	if tenant != "" {
		c["tenant"] = tenant
	}
	return c
}

// TestIssuerTokenThroughConnectEdge drives the production verifier composition
// (buildLoopVerifier from config) through the real Connect server and runtime:
// an issuer-minted token starts a loop owned by its sub, a second user of the
// same issuer is a different owner, a token asserting a tenant the issuer may
// not assert is Unauthenticated, the static token still works, and the dev
// token is not accepted once issuers are configured.
func TestIssuerTokenThroughConnectEdge(t *testing.T) {
	v, usedDefault, err := buildLoopVerifier(issuerTestConfig())
	if err != nil {
		t.Fatalf("buildLoopVerifier: %v", err)
	}
	if usedDefault {
		t.Fatal("issuers configured; the dev-token fallback must not be synthesized")
	}
	base, registry := authTestServer(t, v)
	ctx := context.Background()

	alice := bearerClient(base, mintIssuerToken(t, issuerTestKey, issuerClaims("cms-user-alice", "acme")))
	sr, err := alice.StartLoop(ctx, connect.NewRequest(&loopv1.StartLoopRequest{Task: "hi"}))
	if err != nil {
		t.Fatalf("issuer-token StartLoop: %v", err)
	}
	loopID := sr.Msg.LoopId
	waitForTurnEndByObserve(t, alice, loopID)

	rec, err := registry.Resolve(ctx, event.StreamKey{Tenant: "acme", Loop: event.LoopID(loopID)})
	if err != nil {
		t.Fatalf("resolve loop under acme: %v", err)
	}
	if rec.Owner != "cms-user-alice" {
		t.Fatalf("loop owner = %q, want the token's sub cms-user-alice", rec.Owner)
	}

	// A different CMS user, same issuer and tenant: a different owner.
	bob := bearerClient(base, mintIssuerToken(t, issuerTestKey, issuerClaims("cms-user-bob", "acme")))
	_, err = bob.Send(ctx, connect.NewRequest(&loopv1.SendRequest{LoopId: loopID, Input: "x"}))
	if got := connect.CodeOf(err); got != connect.CodePermissionDenied {
		t.Fatalf("other-subject Send code = %v, want PermissionDenied (err=%v)", got, err)
	}

	refused := map[string]string{
		"tenant the issuer may not assert":     mintIssuerToken(t, issuerTestKey, issuerClaims("cms-user-alice", "globex")),
		"omitted tenant (_default not listed)": mintIssuerToken(t, issuerTestKey, issuerClaims("cms-user-alice", "")),
		"wrong key":                            mintIssuerToken(t, "not-the-shared-secret-but-32-bytes!", issuerClaims("cms-user-alice", "acme")),
		"dev token":                            devToken,
	}
	for name, tok := range refused {
		_, err := bearerClient(base, tok).StartLoop(ctx, connect.NewRequest(&loopv1.StartLoopRequest{Task: "hi"}))
		if got := connect.CodeOf(err); got != connect.CodeUnauthenticated {
			t.Fatalf("%s: StartLoop code = %v, want Unauthenticated (err=%v)", name, got, err)
		}
		var ce *connect.Error
		if !errors.As(err, &ce) || ce.Message() != loopauth.ErrInvalidToken.Error() {
			t.Fatalf("%s: refusal must carry only %q to the client, got %v", name, loopauth.ErrInvalidToken, err)
		}
	}

	static := bearerClient(base, "static-tok")
	sr, err = static.StartLoop(ctx, connect.NewRequest(&loopv1.StartLoopRequest{Task: "hi"}))
	if err != nil {
		t.Fatalf("static-token StartLoop alongside issuers: %v", err)
	}
	waitForTurnEndByObserve(t, static, sr.Msg.LoopId)
}

// TestBuildLoopVerifierIssuerDefaults pins the composition-root defaults: an
// issuer with no tenants may assert only _default, and an issuers-only config
// (no static auth) does not synthesize the dev token.
func TestBuildLoopVerifierIssuerDefaults(t *testing.T) {
	v, usedDefault, err := buildLoopVerifier(config.Config{LoopServer: config.LoopServerConfig{
		Issuers: []config.IssuerConfig{{Issuer: "cms", SigningKey: issuerTestKey, Audience: "fuse-loop"}},
	}})
	if err != nil {
		t.Fatalf("buildLoopVerifier: %v", err)
	}
	if usedDefault {
		t.Fatal("issuers-only config must not synthesize the dev token")
	}
	ctx := context.Background()
	if _, err := v.Verify(ctx, devToken); err == nil {
		t.Fatal("dev token accepted with issuers configured")
	}
	p, err := v.Verify(ctx, mintIssuerToken(t, issuerTestKey, issuerClaims("u1", "")))
	if err != nil {
		t.Fatalf("omitted tenant under a tenant-less issuer: %v", err)
	}
	if p.Tenant != event.DefaultTenant || p.Subject != "u1" || p.ObservabilityOperator {
		t.Fatalf("principal = %+v, want {_default u1 false}", p)
	}
	if _, err := v.Verify(ctx, mintIssuerToken(t, issuerTestKey, issuerClaims("u1", "acme"))); err == nil {
		t.Fatal("a tenant-less issuer asserted tenant acme")
	}

	// The default max_ttl is 1h.
	long := issuerClaims("u1", "")
	long["exp"] = time.Now().Add(61 * time.Minute).Unix()
	if _, err := v.Verify(ctx, mintIssuerToken(t, issuerTestKey, long)); err == nil {
		t.Fatal("a token living longer than the default 1h max_ttl was accepted")
	}
}

// TestToolIdentityTenantKeysIncludeIssuerTenants: a principal minted by an
// issuer must find its tenant's tool-identity signing key, or every identity-
// propagating MCP call from it fails closed.
func TestToolIdentityTenantKeysIncludeIssuerTenants(t *testing.T) {
	cfg := issuerTestConfig()
	cfg.LoopServer.Auth = nil
	cfg.LoopServer.Issuers[0].Tenants = []string{"acme", "globex"}
	cfg.ToolIdentity.SigningKey = "raw-key-value"
	keys := toolIdentityTenantKeys(cfg)
	for _, tenant := range []event.TenantID{event.DefaultTenant, "acme", "globex"} {
		if len(keys[tenant]) == 0 {
			t.Fatalf("no tool-identity key for issuer tenant %q (keys: %v)", tenant, keysOf(keys))
		}
	}
}
