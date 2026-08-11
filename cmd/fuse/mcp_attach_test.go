package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
)

// mcpAttach is the single composition-root helper every loop binding calls to
// wire an mcp.Manager: it yields the identity-propagation ManagerOptions AND the
// complete-mediation permissions.Option together, so no binding can attach a
// manager without also wiring the egress seam + TargetMediator. These table tests
// pin its contract across the identity-propagating and non-propagating configs.

// TestMcpAttach_IdentityPropagating: with an identity server + signing key
// configured, mcpAttach returns (a) a non-empty ManagerOption slice (the
// WithCredentialSource seam), (b) a non-nil mediator option, and (c) writes the
// active posture log.
func TestMcpAttach_IdentityPropagating(t *testing.T) {
	cfg := config.Config{
		MCPServers: []config.MCPServerConfig{
			{Name: "api", Transport: "http", Audience: "https://api.example", Auth: config.MCPAuthConfig{Type: "identity", Scopes: []string{"read"}}},
		},
		ToolIdentity: config.ToolIdentityConfig{SigningKey: "local-signing-key-0123456789"},
	}
	var w bytes.Buffer
	opts, mediator := mcpAttach(cfg, &w)
	if len(opts) == 0 {
		t.Fatal("identity-propagating config must yield a WithCredentialSource ManagerOption")
	}
	if mediator == nil {
		t.Fatal("identity-propagating config must yield a TargetMediator permissions.Option")
	}
	if !strings.Contains(w.String(), "tool-identity") {
		t.Fatalf("expected an active posture log line, got %q", w.String())
	}
}

// TestMcpAttach_NonPropagating: with only a legacy static server, mcpAttach wires
// NO credential source and NO mediator (pre-#52 behavior byte-identical).
func TestMcpAttach_NonPropagating(t *testing.T) {
	cfg := config.Config{
		MCPServers: []config.MCPServerConfig{
			{Name: "legacy", Transport: "http", Auth: config.MCPAuthConfig{Type: "bearer", ClientSecret: "x"}},
		},
		ToolIdentity: config.ToolIdentityConfig{SigningKey: "unused"},
	}
	var w bytes.Buffer
	opts, mediator := mcpAttach(cfg, &w)
	if len(opts) != 0 {
		t.Fatalf("non-propagating config must yield no ManagerOptions, got %d", len(opts))
	}
	if mediator != nil {
		t.Fatal("non-propagating config must yield a nil mediator")
	}
}

// TestMcpAttach_IdentityServerNoSigningKeyLogsReason: an identity server with no
// signing key yields no source (fails closed) but logs the human-readable reason —
// mcpAttach never silently swallows the misconfiguration.
func TestMcpAttach_IdentityServerNoSigningKeyLogsReason(t *testing.T) {
	cfg := config.Config{
		MCPServers: []config.MCPServerConfig{
			{Name: "api", Transport: "http", Audience: "https://api", Auth: config.MCPAuthConfig{Type: "identity"}},
		},
	}
	var w bytes.Buffer
	opts, mediator := mcpAttach(cfg, &w)
	if len(opts) != 0 {
		t.Fatal("no signing key ⇒ no source option")
	}
	// The mediator still gates: an identity server is declared even without a key,
	// so complete mediation stays active (an undeclared target is denied).
	if mediator == nil {
		t.Fatal("an identity server declares a target, so the mediator must still gate")
	}
	if !strings.Contains(w.String(), "signing_key") {
		t.Fatalf("expected the missing-signing-key reason in the log, got %q", w.String())
	}
}
