package main

import (
	"context"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/loopauth"
)

// No identity-propagating MCP server ⇒ no source is built (the seam stays inert;
// MCP tools keep their static-token path).
func TestBuildToolIdentitySource_InertWithoutIdentityServer(t *testing.T) {
	cfg := config.Config{
		MCPServers: []config.MCPServerConfig{
			{Name: "legacy", Auth: config.MCPAuthConfig{Type: "bearer", ClientSecret: "x"}},
		},
		ToolIdentity: config.ToolIdentityConfig{SigningKey: "unused"},
	}
	src, reason := buildToolIdentitySource(cfg)
	if src != nil {
		t.Fatal("no identity server ⇒ expected a nil source")
	}
	if reason != "" {
		t.Fatalf("no identity server ⇒ expected no reason, got %q", reason)
	}
}

// An identity server with no signing key ⇒ no source + a reason (fails closed,
// never a silent downgrade to a static token it does not have).
func TestBuildToolIdentitySource_IdentityServerNeedsSigningKey(t *testing.T) {
	cfg := config.Config{
		MCPServers: []config.MCPServerConfig{
			{Name: "api", Transport: "http", Audience: "https://api", Auth: config.MCPAuthConfig{Type: "identity"}},
		},
	}
	src, reason := buildToolIdentitySource(cfg)
	if src != nil {
		t.Fatal("identity server with no signing key ⇒ expected nil source")
	}
	if reason == "" {
		t.Fatal("expected a startup reason explaining the missing signing key")
	}
	if !strings.Contains(reason, "signing_key") {
		t.Fatalf("reason should name the missing signing key, got %q", reason)
	}
}

// The happy path: an identity server + a signing key ⇒ a working CredentialSource
// that mints a per-call, audience-bound delegation token for the local principal.
// This is the composition-root end-to-end assertion (Task 8).
func TestBuildToolIdentitySource_MintsForLocalPrincipal(t *testing.T) {
	cfg := config.Config{
		MCPServers: []config.MCPServerConfig{
			{Name: "api", Transport: "http", Audience: "https://api.example", Auth: config.MCPAuthConfig{Type: "identity", Scopes: []string{"read"}}},
			{Name: "legacy", Transport: "http", Auth: config.MCPAuthConfig{Type: "bearer", ClientSecret: "legacy-tok"}},
		},
		ToolIdentity: config.ToolIdentityConfig{SigningKey: "local-signing-key-0123456789", LocalSubject: "ethan"},
	}
	src, reason := buildToolIdentitySource(cfg)
	if reason != "" {
		t.Fatalf("unexpected reason: %s", reason)
	}
	if src == nil {
		t.Fatal("expected a CredentialSource")
	}

	p := localPrincipal(cfg)
	if p.Subject != "ethan" || p.Tenant != event.DefaultTenant {
		t.Fatalf("localPrincipal = %+v, want subject=ethan tenant=%s", p, event.DefaultTenant)
	}

	// OAuth-tier target mints an identity-carrying, audience-bound credential.
	oauthTarget := cfg.MCPServers[0].ToolIdentityTarget()
	cred, err := src.CredentialFor(context.Background(), p, oauthTarget)
	if err != nil {
		t.Fatalf("CredentialFor(oauth): %v", err)
	}
	if !cred.CarriesIdentity() {
		t.Fatal("identity-tier credential must carry identity")
	}
	if cred.Header() == "" {
		t.Fatal("expected a bearer header for the minted token")
	}

	// The legacy static server reaches its identity-free credential through the
	// same broker.
	staticTarget := cfg.MCPServers[1].ToolIdentityTarget()
	scred, err := src.CredentialFor(context.Background(), p, staticTarget)
	if err != nil {
		t.Fatalf("CredentialFor(static): %v", err)
	}
	if scred.CarriesIdentity() {
		t.Fatal("static credential must not carry identity")
	}
	if scred.Header() != "Bearer legacy-tok" {
		t.Fatalf("static header = %q, want %q", scred.Header(), "Bearer legacy-tok")
	}
}

// The zero local principal is never empty/spoofable — it defaults to "local".
func TestLocalPrincipal_DefaultsToLocal(t *testing.T) {
	p := localPrincipal(config.Config{})
	if p.Subject != "local" {
		t.Fatalf("default local subject = %q, want local", p.Subject)
	}
	if (p == loopauth.Principal{}) {
		t.Fatal("local principal must never be the zero (spoofable) principal")
	}
}

// An identity-propagating server on a stdio transport fails closed with a reason:
// the per-call credential can only ride an HTTP request, so a stdio identity
// server would mint a token that never leaves the process (M1).
func TestBuildToolIdentitySource_StdioIdentityFailsClosed(t *testing.T) {
	cfg := config.Config{
		MCPServers: []config.MCPServerConfig{
			{Name: "api", Transport: "stdio", Audience: "https://api", Auth: config.MCPAuthConfig{Type: "identity"}},
		},
		ToolIdentity: config.ToolIdentityConfig{SigningKey: "k"},
	}
	src, reason := buildToolIdentitySource(cfg)
	if src != nil {
		t.Fatal("a stdio identity server must not yield an active source")
	}
	if reason == "" {
		t.Fatal("expected a startup reason explaining the stdio identity server is inert")
	}
}
