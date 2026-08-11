package main

import (
	"context"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/tools"
)

// identityCfg builds a config with one identity-propagating HTTP MCP server so
// the mediator + source are active.
func identityCfg(servers ...config.MCPServerConfig) config.Config {
	return config.Config{
		MCPServers:   servers,
		ToolIdentity: config.ToolIdentityConfig{SigningKey: "k-for-tests-0123456789abcdef"},
		Permissions:  config.PermissionsConfig{Mode: "off"},
	}
}

// The config mediator permits a non-MCP tool (no downstream target).
func TestConfigMediator_AllowsNonMCPTool(t *testing.T) {
	m := newConfigTargetMediator(identityCfg())
	if ok, _ := m.MediateTarget(context.Background(), "read_file", `{}`); !ok {
		t.Fatal("a non-MCP tool must be permitted (no downstream target)")
	}
}

// A call to an unconfigured MCP server is denied (root-of-trust allowlist).
func TestConfigMediator_DeniesUnknownServer(t *testing.T) {
	m := newConfigTargetMediator(identityCfg(
		config.MCPServerConfig{Name: "known", Transport: "http", Audience: "https://k", Auth: config.MCPAuthConfig{Type: "identity"}},
	))
	if ok, reason := m.MediateTarget(context.Background(), "mcp:evil/do", `{}`); ok {
		t.Fatal("a call to an unconfigured MCP server must be denied")
	} else if !strings.Contains(reason, "not a configured MCP server") {
		t.Fatalf("unexpected reason: %q", reason)
	}
}

// A configured identity server with NO audience is denied (undeclared target).
func TestConfigMediator_DeniesUndeclaredAudience(t *testing.T) {
	m := newConfigTargetMediator(identityCfg(
		config.MCPServerConfig{Name: "api", Transport: "http", Auth: config.MCPAuthConfig{Type: "identity"}}, // no Audience
	))
	if ok, reason := m.MediateTarget(context.Background(), "mcp:api/do", `{}`); ok {
		t.Fatal("an identity server with no audience is an undeclared target and must be denied")
	} else if !strings.Contains(reason, "no audience") {
		t.Fatalf("unexpected reason: %q", reason)
	}
}

// A properly declared identity server is permitted.
func TestConfigMediator_AllowsDeclaredServer(t *testing.T) {
	m := newConfigTargetMediator(identityCfg(
		config.MCPServerConfig{Name: "api", Transport: "http", Audience: "https://api", Auth: config.MCPAuthConfig{Type: "identity"}},
	))
	if ok, _ := m.MediateTarget(context.Background(), "mcp:api/do", `{}`); !ok {
		t.Fatal("a declared identity server must be permitted")
	}
}

// T3 (from review): assert the SHIPPED gate actually carries the mediator when
// identity propagation is active — buildTargetMediator wired into buildGate — so
// the D5 complete-mediation guarantee is real in the binary, not just a seam.
// This is the regression test whose absence let the unwired gap pass before.
func TestBuildGate_WiresMediatorWhenIdentityActive(t *testing.T) {
	cfg := identityCfg(
		config.MCPServerConfig{Name: "api", Transport: "http", Audience: "https://api", Auth: config.MCPAuthConfig{Type: "identity"}},
	)
	reg := newTestModelRegistry()
	toolReg := tools.NewRegistry()
	toolReg.Register(mediationStubTool{name: "mcp:evil/do"})

	gate := buildGate(cfg, toolReg, permissions.AlwaysApprove, reg, nil, nil)
	// A call to an UNCONFIGURED server must be denied by the shipped gate even in
	// ModeOff — proving the mediator is wired, not nil.
	res := gate.Execute(context.Background(), "mcp:evil/do", `{}`)
	if !res.IsError {
		t.Fatal("shipped gate must carry the target mediator: an unconfigured MCP target must be denied")
	}
	if strings.Contains(res.Output, "ran ") {
		t.Fatal("the denied tool must not execute")
	}
}

// With NO identity server, buildTargetMediator is nil and the gate has no
// mediator — pre-#52 behavior (an MCP tool runs under ModeOff).
func TestBuildGate_NoMediatorWithoutIdentity(t *testing.T) {
	cfg := config.Config{
		MCPServers:  []config.MCPServerConfig{{Name: "legacy", Transport: "http", Auth: config.MCPAuthConfig{Type: "bearer", ClientSecret: "x"}}},
		Permissions: config.PermissionsConfig{Mode: "off"},
	}
	if buildTargetMediator(cfg) != nil {
		t.Fatal("no identity server ⇒ buildTargetMediator must be nil (inert)")
	}
	reg := newTestModelRegistry()
	toolReg := tools.NewRegistry()
	toolReg.Register(mediationStubTool{name: "mcp:legacy/do"})
	gate := buildGate(cfg, toolReg, permissions.AlwaysApprove, reg, nil, nil)
	res := gate.Execute(context.Background(), "mcp:legacy/do", `{}`)
	if res.IsError {
		t.Fatalf("without identity, an MCP tool runs under ModeOff unchanged; got error %q", res.Output)
	}
}

type mediationStubTool struct{ name string }

func (s mediationStubTool) Name() string               { return s.name }
func (s mediationStubTool) Description() string        { return "" }
func (s mediationStubTool) Parameters() map[string]any { return map[string]any{} }
func (s mediationStubTool) Execute(context.Context, string) tools.Result {
	return tools.Result{Output: "ran " + s.name}
}

// newTestModelRegistry returns a minimal model.Registry sufficient for buildGate
// (which only needs it for auto-mode classifier construction, unused in ModeOff).
func newTestModelRegistry() *model.Registry {
	return &model.Registry{}
}
