package main

import (
	"encoding/hex"
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/event"
)

// goldenTenantKeyVector pins the per-tenant key derivation to a FIXED value.
//
// cmd/rentals-mcp carries a byte-identical copy of deriveTenantKey (it cannot import
// this one — both are package main), and cmd/rentals-mcp/tenant_key_golden_test.go
// asserts the SAME constants. The pair is the drift guard: the copies are only proven
// equal transitively by examples/wander/browser_identity_test.go, which is behind the
// `browser` build tag and therefore outside `make test`. Editing either derivation
// alone now turns the DEFAULT lane red.
//
// If you change the derivation deliberately, you must update BOTH files — and every
// already-minted token stops verifying.
const (
	goldenSigningKey = "k"
	goldenAcmeKeyHex = "83ac3cff9aac0cdb79f2b1146ec7be39eaa7c699e17a47e649a367776ff46722"
)

func TestDeriveTenantKeyGoldenVector(t *testing.T) {
	got := hex.EncodeToString(deriveTenantKey([]byte(goldenSigningKey), event.TenantID("acme")))
	if got != goldenAcmeKeyHex {
		t.Fatalf("deriveTenantKey drifted:\n got %s\nwant %s\n(cmd/rentals-mcp holds the same vector; both copies must change together)", got, goldenAcmeKeyHex)
	}
}

// TestDefaultTenantUsesRawSigningKey pins the CALLER-side half of the invariant: the
// default tenant is keyed with the RAW signing key, never a derived one. That is the
// divergence that actually bit — the derivation functions agreed while the callers
// did not.
func TestDefaultTenantUsesRawSigningKey(t *testing.T) {
	cfg := config.Config{
		ToolIdentity: config.ToolIdentityConfig{SigningKey: goldenSigningKey},
		LoopServer: config.LoopServerConfig{
			Auth: []config.AuthTokenConfig{{Token: "tok-a", Tenant: "acme", Subject: "alice"}},
		},
	}

	keys := toolIdentityTenantKeys(cfg)

	if got := string(keys[event.DefaultTenant]); got != goldenSigningKey {
		t.Fatalf("default tenant key = %q, want the raw signing key %q", got, goldenSigningKey)
	}
	if got := hex.EncodeToString(keys[event.TenantID("acme")]); got != goldenAcmeKeyHex {
		t.Fatalf("named tenant key = %s, want the derived vector %s", got, goldenAcmeKeyHex)
	}
}
