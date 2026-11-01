package main

import (
	"context"
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/loopauth"
)

// TestBuildLoopVerifierStaticScopes proves a static loop_server.auth entry's
// scopes reach the Principal through the real composition root, canonicalized,
// and that an entry without scopes stays unrestricted.
func TestBuildLoopVerifierStaticScopes(t *testing.T) {
	v, _, err := buildLoopVerifier(config.Config{LoopServer: config.LoopServerConfig{Auth: []config.AuthTokenConfig{
		{Token: "a", Tenant: "acme", Subject: "x", Scopes: []string{"loop:observe"}},
		{Token: "b", Tenant: "acme", Subject: "y"},
	}}})
	if err != nil {
		t.Fatalf("buildLoopVerifier: %v", err)
	}
	pa, err := v.Verify(context.Background(), "a")
	if err != nil {
		t.Fatalf("Verify(a): %v", err)
	}
	if want := loopauth.NewScopes("loop:observe"); pa.Scopes != want {
		t.Errorf("a.Scopes = %q, want %q", pa.Scopes, want)
	}
	pb, err := v.Verify(context.Background(), "b")
	if err != nil {
		t.Fatalf("Verify(b): %v", err)
	}
	if pb.Scopes != "" {
		t.Errorf("b.Scopes = %q, want unrestricted", pb.Scopes)
	}
}
