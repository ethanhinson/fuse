package main

import (
	"encoding/hex"
	"testing"

	"github.com/ethanhinson/fuse/internal/event"
)

// goldenTenantKeyVector is the OTHER half of the cross-package parity guard. The
// same two constants appear in cmd/fuse/tenant_key_golden_test.go; deriveTenantKey
// here is a hand copy of cmd/fuse's (both are package main, so neither can import
// the other) and the two MUST stay byte-identical or no token this server sees will
// verify.
//
// Before this pair existed, the invariant was only proven transitively by
// examples/wander/browser_identity_test.go, which is `//go:build browser` and so
// outside `make test` — a one-sided edit went green on the default lane.
const (
	goldenSigningKey = "k"
	goldenAcmeKeyHex = "83ac3cff9aac0cdb79f2b1146ec7be39eaa7c699e17a47e649a367776ff46722"
)

func TestDeriveTenantKeyGoldenVector(t *testing.T) {
	got := hex.EncodeToString(deriveTenantKey([]byte(goldenSigningKey), event.TenantID("acme")))
	if got != goldenAcmeKeyHex {
		t.Fatalf("deriveTenantKey drifted:\n got %s\nwant %s\n(cmd/fuse holds the same vector; both copies must change together)", got, goldenAcmeKeyHex)
	}
}

// TestBuildTenantKeysMatchesFuseCallers pins the CALLER-side half: `_default` takes
// the RAW signing key (no derivation), exactly as cmd/fuse's toolIdentityTenantKeys
// seeds it, while a named tenant takes the derived vector. This caller divergence is
// the one that actually bit — the derivation functions agreed all along.
func TestBuildTenantKeysMatchesFuseCallers(t *testing.T) {
	keys, err := buildTenantKeys(goldenSigningKey, "acme,_default", nil)
	if err != nil {
		t.Fatalf("buildTenantKeys: %v", err)
	}
	if got := string(keys[event.DefaultTenant]); got != goldenSigningKey {
		t.Fatalf("_default key = %q, want the raw signing key %q", got, goldenSigningKey)
	}
	if got := hex.EncodeToString(keys[event.TenantID("acme")]); got != goldenAcmeKeyHex {
		t.Fatalf("acme key = %s, want the derived vector %s", got, goldenAcmeKeyHex)
	}
}
