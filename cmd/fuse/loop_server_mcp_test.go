package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/runtime"
	"github.com/ethanhinson/fuse/internal/toolidentity"
	"github.com/ethanhinson/fuse/internal/tools"
)

// loopServerMCPCfg builds a loop-server config with one identity-propagating MCP
// server pointed at url, plus the tenants declared under loop_server.auth so the STS
// is keyed for them — buildLoopServerRuntimeDeps then attaches a per-loop manager
// that mints per-principal tokens for those real tenants.
func loopServerMCPCfg(url string, tenants ...string) config.Config {
	var auth []config.AuthTokenConfig
	for _, ten := range tenants {
		auth = append(auth, config.AuthTokenConfig{Token: "tok-" + ten, Tenant: ten, Subject: "user-" + ten})
	}
	return config.Config{
		MCPServers: []config.MCPServerConfig{{
			Name:      "stub",
			Transport: "sse",
			URL:       url,
			Audience:  "https://stub.example",
			Auth:      config.MCPAuthConfig{Type: "identity"},
		}},
		ToolIdentity: config.ToolIdentityConfig{SigningKey: "loop-mcp-signing-key-0123456789abcdef"},
		LoopServer:   config.LoopServerConfig{Auth: auth},
	}
}

// TestLoopServer_AttachesPerLoopMCPManager is the Task-4 crux: the loop-server
// attaches a per-loop MCP manager whose tool is registered into THAT loop's own
// cloned registry. Two concurrent loops each get their OWN manager + registry — a
// tool registered for loop A is not visible to loop B, and no shared manager. Driven
// concurrently under -race so a cross-loop share is observable.
func TestLoopServer_AttachesPerLoopMCPManager(t *testing.T) {
	stub := newStubMCPServer(t, "do_stub")
	cfg := loopServerMCPCfg(stub.URL())
	reg := newTestModelRegistry()

	deps := buildLoopServerRuntimeDeps(nil, cfg, reg, "cloud/x", defaultToolRegistry(nil, cfg.Research, nil),
		spawnAgentBlock, permissions.AlwaysApprove, nil)
	if deps.NewToolRegistry == nil || deps.LoopTeardown == nil {
		t.Fatal("loop-server Deps must build per-loop registries and a LoopTeardown")
	}

	const n = 4
	var wg sync.WaitGroup
	regs := make([]*tools.Registry, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			loopReg := deps.NewToolRegistry()
			regs[i] = loopReg
			if !loopReg.Has("mcp:stub/do_stub") {
				t.Errorf("loop %d: per-loop registry is missing the attached MCP tool (manager not attached to THIS loop's registry)", i)
			}
		}(i)
	}
	wg.Wait()
	defer func() {
		for _, r := range regs {
			deps.LoopTeardown(r)
		}
	}()

	// Distinct registry instances: no two loops share one.
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			if regs[i] == regs[j] {
				t.Fatalf("loops %d and %d share ONE registry — per-loop isolation broken", i, j)
			}
		}
	}

	// Isolation: unregister the MCP tool from loop 0's registry; loop 1 must be
	// unaffected (its own manager/registry).
	regs[0].Unregister("mcp:stub/do_stub")
	if regs[0].Has("mcp:stub/do_stub") {
		t.Fatal("loop 0 tool should be gone after unregister")
	}
	if !regs[1].Has("mcp:stub/do_stub") {
		t.Fatal("loop 1 must still have its tool — registries are not shared")
	}
}

// TestLoopServer_MCPToolInvokableWithRealPrincipal drives the full path: a loop
// started on the loop-server (via StartLoop, LoopContext stamping the real
// principal) can INVOKE the attached MCP tool, and the outbound call mints a
// per-principal delegation token that reaches the stub server over the wire. This
// proves the per-loop manager + the Task-3 principal threading compose end to end.
func TestLoopServer_MCPToolInvokableWithRealPrincipal(t *testing.T) {
	stub := newStubMCPServer(t, "do_stub")
	// Key the acme tenant (declared under loop_server.auth) so the STS can mint for it.
	cfg := loopServerMCPCfg(stub.URL(), "acme")
	reg := newTestModelRegistry()

	deps := buildLoopServerRuntimeDeps(nil, cfg, reg, "cloud/x", defaultToolRegistry(nil, cfg.Research, nil),
		spawnAgentBlock, permissions.AlwaysApprove, nil)

	// Build one loop's registry (manager attached) and invoke the MCP tool directly
	// through the registry under a principal-carrying context — exactly what the agent
	// engine does at a tool call. The stub records the call, proving the wire path ran.
	loopReg := deps.NewToolRegistry()
	defer deps.LoopTeardown(loopReg)
	base := deps.LoopContext(context.Background(), runtime.LoopConfig{Tenant: "acme", Subject: "alice"})
	if _, ok := toolidentity.PrincipalFrom(base); !ok {
		t.Fatal("LoopContext must have stamped the principal")
	}
	// Bounded context: a stub miss fails fast rather than hanging the suite.
	ctx, cancel := context.WithTimeout(base, 10*time.Second)
	defer cancel()
	res := loopReg.Execute(ctx, "mcp:stub/do_stub", `{}`)
	if res.IsError {
		t.Fatalf("MCP tool invocation failed: %s", res.Output)
	}
	if stub.callCount() == 0 {
		t.Fatal("the stub server recorded no tools/call — the wire round-trip did not happen")
	}
}
