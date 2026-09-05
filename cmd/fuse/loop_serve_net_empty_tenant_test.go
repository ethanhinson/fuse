package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/loopauth"
	"github.com/ethanhinson/fuse/internal/tools/sandbox"
)

// TestBuildLoopVerifierCollapsesEmptyTenantAtTheAuthEdge is the regression test for
// the change-0065 review BLOCKER: a `loop_server.auth` entry with `tenant` omitted
// is a DOCUMENTED, LOADER-PINNED config shape (internal/config/schema.go's
// AuthTokenConfig, and TestLoopServerAuthLoadsFromTrustedHome, which asserts such an
// entry survives Load with Tenant == ""). Before the fix buildLoopVerifier passed
// that "" through verbatim, and under the new hosted wiring the resulting Principal
// hit TenantRoots.Root -> tenantDirName("") -> !ok -> ErrNoTenantRoot, which
// containerHandler.Acquire turns into a hard failure: EVERY bash call from that
// principal returned "bash is unavailable".
//
// The collapse now happens ONCE, at the trusted authentication edge, which is where
// binding #2 (loop_server.go's principalFromConfig) and the tool-identity key
// derivation (tool_identity.go's toolIdentityTenantKeys — reading this SAME auth
// list) have always done it. The resolver's refuse-empty invariant is untouched: it
// still refuses "", it simply never sees one from this edge again.
func TestBuildLoopVerifierCollapsesEmptyTenantAtTheAuthEdge(t *testing.T) {
	v, usedDefault := buildLoopVerifier(config.Config{LoopServer: config.LoopServerConfig{Auth: []config.AuthTokenConfig{
		{Token: "tok-acme", Tenant: "acme", Subject: "alice"},
		{Token: "tok-bare", Subject: "bob"}, // tenant omitted: the documented shape
	}}})
	if usedDefault {
		t.Fatal("auth entries were configured; the dev-token fallback must not be synthesized")
	}

	bare, err := v.Verify(context.Background(), "tok-bare")
	if err != nil {
		t.Fatalf("verify tok-bare: %v", err)
	}
	if bare.Tenant != event.DefaultTenant {
		t.Errorf("empty-tenant auth entry resolved to Tenant %q, want %q — an un-collapsed empty tenant reaches the mount resolver and refuses every bash call",
			bare.Tenant, event.DefaultTenant)
	}
	if bare.Subject != "bob" {
		t.Errorf("Subject = %q, want bob", bare.Subject)
	}

	// An explicitly-named tenant must pass through completely untouched: the
	// collapse is one-directional and applies ONLY to the absent identity.
	acme, err := v.Verify(context.Background(), "tok-acme")
	if err != nil {
		t.Fatalf("verify tok-acme: %v", err)
	}
	if acme.Tenant != event.TenantID("acme") {
		t.Errorf("named tenant = %q, want acme", acme.Tenant)
	}

	// THE PROPERTY THAT ACTUALLY BROKE. A verifier-issued Principal must resolve to
	// a real per-tenant mount root under the hosted wiring. This is the end-to-end
	// assertion: asserting only that a string was rewritten would not have caught
	// the blocker, because the blocker was that the string reached the resolver.
	parent := t.TempDir()
	roots := sandbox.NewTenantRoots(parent, true)

	bareRoot, err := roots.Root(bare)
	if err != nil {
		t.Fatalf("the empty-tenant principal has no workspace root (%v); every bash call from it would refuse", err)
	}
	acmeRoot, err := roots.Root(acme)
	if err != nil {
		t.Fatalf("named-tenant root: %v", err)
	}
	if bareRoot == acmeRoot {
		t.Fatal("the collapsed principal and tenant acme resolved to the SAME tree — a cross-tenant merge")
	}
	if got, want := filepath.Base(bareRoot), string(event.DefaultTenant); got != want {
		t.Errorf("collapsed principal's tree is %q, want a directory named %q", got, want)
	}

	// The resolver's refuse-empty floor STAYS. The fix moved the collapse to the
	// authentication boundary; it did not weaken the security invariant beneath it.
	if _, err := roots.Root(loopauth.Principal{Tenant: ""}); !errors.Is(err, sandbox.ErrNoTenantRoot) {
		t.Errorf("TenantRoots.Root(\"\") = %v, want ErrNoTenantRoot — the refuse-empty invariant must remain intact", err)
	}
}
