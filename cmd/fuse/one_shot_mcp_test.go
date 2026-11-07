package main

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/permissions"
)

// TestOneShot_AttachesMCPViaSharedHelper: the one-shot deps attach an MCP manager and
// can invoke an MCP tool over the wire.
func TestOneShot_AttachesMCPViaSharedHelper(t *testing.T) {
	stub := newStubMCPServer(t, "do_stub")
	cfg := config.Config{
		MCPServers: []config.MCPServerConfig{{
			Name:      "stub",
			Transport: "sse",
			URL:       stub.URL(),
			Auth:      config.MCPAuthConfig{Type: "none"},
		}},
	}
	reg := newTestModelRegistry()
	toolReg := defaultToolRegistry(nil, cfg.Research, nil)
	tree := agent.NewAgentTreeWithConcurrency("cloud/x", "cloud/x", 1)

	deps, oneShotMCPClose := buildOneShotRuntimeDeps(cfg, reg, "cloud/x", toolReg, tree, io.Discard, false, nil,
		permissions.AlwaysApprove, spawnAgentBlock, false, nil, nil)

	// The MCP tool is attached to the one-shot registry and invokable over the wire.
	if !toolReg.Has("mcp:stub/do_stub") {
		t.Fatal("one-shot registry is missing the attached MCP tool")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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
	// The returned close func is idempotent (change #59 review S2): calling it after the
	// LoopTeardown above already closed the manager must be a harmless no-op, not a panic.
	oneShotMCPClose()
}
