package main

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/runtime"
	"github.com/ethanhinson/fuse/internal/toolidentity"
)

// readMainGo returns the source of main.go for the non-goal flag guard.
func readMainGo() (string, error) {
	b, err := os.ReadFile("main.go")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// TestOneShot_AttachesMCPViaSharedHelper is the Task-5 proof: the one-shot deps
// attaches an MCP manager through mcpAttach and can INVOKE an MCP tool, seeding the
// loop initiator as localPrincipal(cfg) (→ DefaultTenant) via LoopContext — exactly
// mirroring the shell's local-identity model, no new CLI identity flag.
func TestOneShot_AttachesMCPViaSharedHelper(t *testing.T) {
	stub := newStubMCPServer(t, "do_stub")
	cfg := config.Config{
		MCPServers: []config.MCPServerConfig{{
			Name:      "stub",
			Transport: "sse",
			URL:       stub.URL(),
			Audience:  "https://stub.example",
			Auth:      config.MCPAuthConfig{Type: "identity"},
		}},
		ToolIdentity: config.ToolIdentityConfig{SigningKey: "one-shot-signing-key-0123456789abcdef", LocalSubject: "ethan"},
	}
	reg := newTestModelRegistry()
	toolReg := defaultToolRegistry(cfg.Research, nil)
	tree := agent.NewAgentTreeWithConcurrency("cloud/x", "cloud/x", 1)

	deps := buildOneShotRuntimeDeps(cfg, reg, "cloud/x", toolReg, tree, io.Discard, false, nil,
		permissions.AlwaysApprove, spawnAgentBlock, false, nil)

	if deps.LoopContext == nil {
		t.Fatal("one-shot Deps must set LoopContext seeding localPrincipal")
	}
	// The stamped principal is the local one (subject from config, DefaultTenant).
	ctxBase := deps.LoopContext(context.Background(), runtime.LoopConfig{})
	p, ok := toolidentity.PrincipalFrom(ctxBase)
	if !ok {
		t.Fatal("LoopContext must stamp the local principal")
	}
	want := localPrincipal(cfg)
	if p != want {
		t.Fatalf("one-shot principal = %+v, want localPrincipal %+v", p, want)
	}

	// The MCP tool is attached to the one-shot registry and invokable over the wire.
	if !toolReg.Has("mcp:stub/do_stub") {
		t.Fatal("one-shot registry is missing the attached MCP tool")
	}
	ctx, cancel := context.WithTimeout(ctxBase, 10*time.Second)
	defer cancel()
	res := toolReg.Execute(ctx, "mcp:stub/do_stub", `{}`)
	if res.IsError {
		t.Fatalf("one-shot MCP tool invocation failed: %s", res.Output)
	}
	if stub.callCount() == 0 {
		t.Fatal("the stub recorded no tools/call — the one-shot wire round-trip did not happen")
	}
	if deps.LoopTeardown != nil {
		deps.LoopTeardown(toolReg)
	}
}

// TestOneShot_NoTenantOrPrincipalFlag is the explicit non-goal guard (spec §3): the
// one-shot identity is the config-derived localPrincipal ONLY — there is no
// per-invocation --tenant/--principal override plumbed into it. localPrincipal reads
// solely cfg.ToolIdentity, never a flag, so no CLI identity surface exists.
func TestOneShot_NoTenantOrPrincipalFlag(t *testing.T) {
	cfg := config.Config{ToolIdentity: config.ToolIdentityConfig{LocalSubject: "x"}}
	if got := localPrincipal(cfg).Subject; got != "x" {
		t.Fatalf("local principal must derive from config only; got subject %q", got)
	}
	// The run() flag surface (main.go) must not define --tenant/--principal. Assert
	// against the source of the flag definitions so a future addition trips this.
	src := oneShotFlagDefinitions(t)
	if strings.Contains(src, "\"tenant\"") || strings.Contains(src, "\"principal\"") {
		t.Fatal("one-shot must not define a --tenant/--principal flag (explicit non-goal)")
	}
}

// oneShotFlagDefinitions reads the run() flag-definition block from main.go so the
// non-goal guard trips if a --tenant/--principal flag is ever added there.
func oneShotFlagDefinitions(t *testing.T) string {
	t.Helper()
	b, err := readMainGo()
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	return b
}
