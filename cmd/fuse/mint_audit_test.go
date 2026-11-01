package main

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/harnessresolve"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/runtime"
	"github.com/ethanhinson/fuse/internal/toolidentity"
	"github.com/ethanhinson/fuse/internal/tools"
)

// mintedEvents returns every tool.identity.minted event in store, in order.
func mintedEvents(t *testing.T, store *event.MemStore) []event.Event {
	t.Helper()
	evs, err := store.Replay(0)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	var out []event.Event
	for _, e := range evs {
		if e.Kind == event.KindToolIdentityMinted {
			out = append(out, e)
		}
	}
	return out
}

// mintedPayload decodes a minted event's payload, asserting it holds exactly the
// four audit keys and nothing else (no token material can ride along).
func mintedPayload(t *testing.T, e event.Event) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(e.Payload, &m); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if got := strings.Join(keys, ","); got != "audience,loop_id,scope,subject" {
		t.Fatalf("payload keys = %s, want exactly audience,loop_id,scope,subject", got)
	}
	return m
}

func TestLoopMintAuditRecordsOAuthMintsToBoundLoop(t *testing.T) {
	a := newLoopMintAudit()
	store := event.NewMemStore(0)
	reg := tools.NewRegistry()
	a.bind(reg, "loop-1", store)

	a.Record(toolidentity.AuditRecord{
		Subject: "alice", Tenant: "acme", Audience: "https://stub.example",
		Scopes: []string{"read", "write"}, Tier: "oauth", Loop: "loop-1",
	})

	got := mintedEvents(t, store)
	if len(got) != 1 {
		t.Fatalf("minted events = %d, want 1", len(got))
	}
	if got[0].NodeID != "loop-1" {
		t.Fatalf("NodeID = %q, want loop-1", got[0].NodeID)
	}
	m := mintedPayload(t, got[0])
	want := map[string]any{"subject": "alice", "audience": "https://stub.example", "scope": "read write", "loop_id": "loop-1"}
	for k, v := range want {
		if m[k] != v {
			t.Fatalf("payload[%s] = %v, want %v", k, m[k], v)
		}
	}
}

func TestLoopMintAuditIgnoresStaticUnknownAndUnbound(t *testing.T) {
	a := newLoopMintAudit()
	store := event.NewMemStore(0)
	reg := tools.NewRegistry()
	a.bind(reg, "loop-1", store)

	base := toolidentity.AuditRecord{Subject: "alice", Tenant: "acme", Audience: "https://stub.example", Tier: "oauth", Loop: "loop-1"}

	static := base
	static.Tier = "static"
	a.Record(static)

	other := base
	other.Loop = "other"
	a.Record(other)

	none := base
	none.Loop = ""
	a.Record(none)

	if n := len(mintedEvents(t, store)); n != 0 {
		t.Fatalf("static/unknown/loopless records appended %d events, want 0", n)
	}

	a.unbind(reg)
	a.Record(base)
	if n := len(mintedEvents(t, store)); n != 0 {
		t.Fatalf("a record for an unbound loop appended %d events, want 0", n)
	}

	// A loop id re-bound by a newer registry (a resume) keeps routing to the
	// newer store when the older registry is torn down afterwards.
	reg2 := tools.NewRegistry()
	store2 := event.NewMemStore(0)
	a.bind(reg, "loop-1", store)
	a.bind(reg2, "loop-1", store2)
	a.unbind(reg)
	a.Record(base)
	if n := len(mintedEvents(t, store2)); n != 1 {
		t.Fatalf("after the stale registry's unbind, the rebound loop recorded %d events, want 1", n)
	}
	if n := len(mintedEvents(t, store)); n != 0 {
		t.Fatalf("the stale store received %d events, want 0", n)
	}

	var nilAudit *loopMintAudit
	nilAudit.bind(reg, "loop-1", store)
	nilAudit.Record(base)
	nilAudit.unbind(reg)
}

// TestLoopServerMintEventsReachTheLoopStream is the composition-root guard: the
// sink is constructed and bound by buildLoopServerRuntimeDeps itself, so a real
// mint through a loop's MCP tool lands in THAT loop's own store.
func TestLoopServerMintEventsReachTheLoopStream(t *testing.T) {
	stub := newStubMCPServer(t, "do_stub")
	cfg := loopServerMCPCfg(stub.URL(), "acme")
	// A real model registry, because BuildAgent resolves the model alias. No
	// model request is ever made: the test drives the tool registry directly.
	t.Setenv("LLM_GATEWAY_URL", "http://127.0.0.1:1")
	t.Setenv("LLM_GATEWAY_KEY", "tkn")
	modelCfg, err := config.Load()
	if err != nil {
		t.Fatalf("config load: %v", err)
	}
	reg := registryFromConfig(modelCfg)
	alias := reg.Default
	deps := buildLoopServerRuntimeDeps(nil, cfg, reg, alias,
		defaultToolRegistry(nil, cfg.Research, nil), spawnAgentBlock, permissions.AlwaysApprove, nil)

	loopReg := deps.NewToolRegistry()
	defer deps.LoopTeardown(loopReg)
	store := event.NewMemStore(0)
	tree := agent.NewAgentTreeWithRootID("root", alias, 1, "loop-mint-1")
	if _, _, _, err := deps.BuildAgent(store, tree, alias, loopReg); err != nil {
		t.Fatalf("BuildAgent: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ctx = deps.LoopContext(ctx, runtime.LoopConfig{Tenant: "acme", Subject: "alice", LoopID: "loop-mint-1"})
	for i := 0; i < 2; i++ {
		if res := loopReg.Execute(ctx, "mcp:stub/do_stub", "{}"); res.IsError {
			t.Fatalf("call %d: %s", i, res.Output)
		}
	}

	got := mintedEvents(t, store)
	if len(got) != 2 {
		t.Fatalf("minted events = %d, want 2", len(got))
	}
	for _, e := range got {
		m := mintedPayload(t, e)
		if m["subject"] != "alice" || m["audience"] != "https://stub.example" || m["loop_id"] != "loop-mint-1" {
			t.Fatalf("minted payload = %v", m)
		}
	}
	all, _ := store.Replay(0)
	for _, e := range all {
		if strings.Contains(string(e.Payload), "eyJ") {
			t.Fatalf("event %s carries token material: %s", e.Kind, e.Payload)
		}
	}
}

// TestHarnessLoopSettingsCarryMintAudit: a loop built from a harness spec gets
// its MCP attach options from settingsFor, the second attach site on the loop
// server. A mint through those options must reach the host's sink.
func TestHarnessLoopSettingsCarryMintAudit(t *testing.T) {
	stub := newStubMCPServer(t, "do_stub")
	cfg := loopServerMCPCfg(stub.URL(), "acme")
	deps := buildLoopServerRuntimeDeps(nil, config.Config{}, newTestModelRegistry(), "cloud/x",
		defaultToolRegistry(nil, config.Config{}.Research, nil), spawnAgentBlock, permissions.AlwaysApprove, nil)
	host, ok := deps.Harness.(*loopServerHost)
	if !ok {
		t.Fatalf("deps.Harness = %T, want *loopServerHost", deps.Harness)
	}

	f := host.factory(host.settingsFor(harnessresolve.Resolved{Config: cfg}))
	loopReg := f.NewToolRegistry()
	defer host.teardown(loopReg)
	store := event.NewMemStore(0)
	host.mintAudit.bind(loopReg, "loop-h", store)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ctx = deps.LoopContext(ctx, runtime.LoopConfig{Tenant: "acme", Subject: "alice", LoopID: "loop-h"})
	if res := loopReg.Execute(ctx, "mcp:stub/do_stub", "{}"); res.IsError {
		t.Fatalf("call: %s", res.Output)
	}
	if n := len(mintedEvents(t, store)); n != 1 {
		t.Fatalf("harness-loop minted events = %d, want 1 (settingsFor must attach the host's audit sink)", n)
	}
}
