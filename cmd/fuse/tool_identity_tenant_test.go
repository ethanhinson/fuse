package main

import (
	"context"
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/loopauth"
	"github.com/ethanhinson/fuse/internal/toolidentity"
)

// loopServerIdentityCfg builds a loop-server config that declares two tenants via
// loop_server.auth PLUS an identity-propagating MCP server + a signing key, so the
// built-in STS must mint for the real per-loop tenants (not DefaultTenant only).
func loopServerIdentityCfg() config.Config {
	return config.Config{
		MCPServers: []config.MCPServerConfig{
			{Name: "api", Transport: "http", Audience: "https://api.example", Auth: config.MCPAuthConfig{Type: "identity", Scopes: []string{"read"}}},
		},
		ToolIdentity: config.ToolIdentityConfig{SigningKey: "loop-server-signing-key-abcdef0123456789"},
		LoopServer: config.LoopServerConfig{
			Auth: []config.AuthTokenConfig{
				{Token: "tok-a", Tenant: "acme", Subject: "alice"},
				{Token: "tok-b", Tenant: "globex", Subject: "bob"},
			},
		},
	}
}

// TestBuildToolIdentitySource_MintsForNonDefaultTenant proves the STS is no longer
// DefaultTenant-only: it mints a valid, tenant-distinct delegation token for a
// non-_default tenant declared under loop_server.auth.
func TestBuildToolIdentitySource_MintsForNonDefaultTenant(t *testing.T) {
	cfg := loopServerIdentityCfg()
	src, reason := buildToolIdentitySource(cfg)
	if reason != "" {
		t.Fatalf("unexpected reason: %s", reason)
	}
	if src == nil {
		t.Fatal("expected a CredentialSource for a multi-tenant loop-server config")
	}
	target := cfg.MCPServers[0].ToolIdentityTarget()

	for _, tenant := range []event.TenantID{"acme", "globex", event.DefaultTenant} {
		p := loopauth.Principal{Tenant: tenant, Subject: "user"}
		cred, err := src.CredentialFor(context.Background(), p, target)
		if err != nil {
			t.Fatalf("tenant %q: CredentialFor failed (STS not keyed for it?): %v", tenant, err)
		}
		if !cred.CarriesIdentity() || cred.Header() == "" {
			t.Fatalf("tenant %q: expected an identity-carrying minted token", tenant)
		}
	}
}

// TestBuildToolIdentitySource_TenantKeysDoNotCross is the per-tenant credential
// isolation guard (D4): a token minted for tenant A must NOT verify under tenant
// B's key. It reaches into the derived per-tenant keys the composition root builds
// and rebuilds an STS from them to cross-verify.
func TestBuildToolIdentitySource_TenantKeysDoNotCross(t *testing.T) {
	cfg := loopServerIdentityCfg()
	keys := toolIdentityTenantKeys(cfg)
	if len(keys) < 3 {
		t.Fatalf("expected keys for acme, globex, and _default; got %d: %v", len(keys), keysOf(keys))
	}
	// Two distinct tenants must have distinct signing material.
	if string(keys["acme"]) == string(keys["globex"]) {
		t.Fatal("distinct tenants must derive DISTINCT signing keys")
	}
	if string(keys["acme"]) == string(keys[event.DefaultTenant]) {
		t.Fatal("a named tenant must not collide with the _default key")
	}

	sts, err := toolidentity.NewBuiltinSTS(toolidentity.BuiltinSTSConfig{Issuer: "fuse", TenantKeys: keys})
	if err != nil {
		t.Fatalf("NewBuiltinSTS: %v", err)
	}
	res, err := sts.Exchange(context.Background(), toolidentity.ExchangeRequest{
		Tenant: "acme", Subject: "alice", Actor: "fuse", Audience: "https://api.example",
	})
	if err != nil {
		t.Fatalf("Exchange for acme: %v", err)
	}
	// The acme token verifies under acme's key...
	if err := sts.Verify("acme", res.Token); err != nil {
		t.Fatalf("acme token must verify under acme key: %v", err)
	}
	// ...but MUST NOT verify under globex's key (cross-tenant isolation).
	if err := sts.Verify("globex", res.Token); err == nil {
		t.Fatal("an acme-minted token must NOT verify under globex's key (cross-tenant leak)")
	}
}

// TestToolIdentityTenantKeys_DefaultUnchanged is the shell/one-shot regression
// guard: with no loop_server.auth, the _default key is exactly the raw signing key
// (byte-identical to the pre-#59 single-tenant path).
func TestToolIdentityTenantKeys_DefaultUnchanged(t *testing.T) {
	cfg := config.Config{ToolIdentity: config.ToolIdentityConfig{SigningKey: "raw-key-value"}}
	keys := toolIdentityTenantKeys(cfg)
	if len(keys) != 1 {
		t.Fatalf("no loop_server.auth ⇒ exactly one (_default) key, got %d", len(keys))
	}
	if string(keys[event.DefaultTenant]) != "raw-key-value" {
		t.Fatalf("_default key must be the raw signing key verbatim, got %q", string(keys[event.DefaultTenant]))
	}
}

func keysOf(m map[event.TenantID][]byte) []event.TenantID {
	out := make([]event.TenantID, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
