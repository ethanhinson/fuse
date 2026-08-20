package main

import (
	"context"
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/runtime"
	"github.com/ethanhinson/fuse/internal/toolidentity"
)

// TestLoopServerDeps_StampsRealPrincipal is the Task-3 crux proof: the loop-server
// Deps.LoopContext hook stamps the loop's authenticated initiator principal (tenant
// + subject from the Connect edge, carried on LoopConfig) onto the run context via
// toolidentity.WithPrincipal — so MCPTool.Execute mints for the REAL user, per
// tenant, never a seeded DefaultTenant local default. Two loops by different
// principals must resolve to their OWN principal and never cross.
func TestLoopServerDeps_StampsRealPrincipal(t *testing.T) {
	cfg := config.Config{}
	reg := newTestModelRegistry()
	deps := buildLoopServerRuntimeDeps(nil, cfg, reg, "cloud/x", defaultToolRegistry(nil, cfg.Research, nil),
		spawnAgentBlock, permissions.AlwaysApprove, nil)

	if deps.LoopContext == nil {
		t.Fatal("loop-server Deps must set LoopContext to thread the real principal")
	}

	// Loop A: authenticated as (acme, alice).
	ctxA := deps.LoopContext(context.Background(), runtime.LoopConfig{Tenant: "acme", Subject: "alice"})
	pA, okA := toolidentity.PrincipalFrom(ctxA)
	if !okA {
		t.Fatal("LoopContext must stamp a Principal for loop A")
	}
	if pA.Tenant != "acme" || pA.Subject != "alice" {
		t.Fatalf("loop A principal = %+v, want {acme alice}", pA)
	}

	// Loop B: authenticated as (globex, bob) — must NOT see A's principal.
	ctxB := deps.LoopContext(context.Background(), runtime.LoopConfig{Tenant: "globex", Subject: "bob"})
	pB, _ := toolidentity.PrincipalFrom(ctxB)
	if pB.Tenant != "globex" || pB.Subject != "bob" {
		t.Fatalf("loop B principal = %+v, want {globex bob}", pB)
	}
	if pB == pA {
		t.Fatal("two loops by different principals must never share a principal")
	}
}

// TestLoopServerDeps_FallsBackToTenantLocalPrincipal: when no authenticated subject
// is present (an un-intercepted/registry-less path — Subject empty), the hook still
// stamps a non-empty, non-spoofable principal scoped to the loop's tenant, so an MCP
// call never mints under the zero principal.
func TestLoopServerDeps_FallsBackToTenantLocalPrincipal(t *testing.T) {
	cfg := config.Config{}
	reg := newTestModelRegistry()
	deps := buildLoopServerRuntimeDeps(nil, cfg, reg, "cloud/x", defaultToolRegistry(nil, cfg.Research, nil),
		spawnAgentBlock, permissions.AlwaysApprove, nil)

	ctx := deps.LoopContext(context.Background(), runtime.LoopConfig{Tenant: event.DefaultTenant, Subject: ""})
	p, ok := toolidentity.PrincipalFrom(ctx)
	if !ok {
		t.Fatal("LoopContext must always stamp a principal, even with no subject")
	}
	if p.Subject == "" {
		t.Fatal("the fallback principal must carry a non-empty subject (never spoofable-zero)")
	}
	if p.Tenant != event.DefaultTenant {
		t.Fatalf("fallback tenant = %q, want _default", p.Tenant)
	}
}
