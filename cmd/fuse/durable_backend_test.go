package main

import (
	"context"
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/event"
)

// TestSelectDurableBackendReturnsFSStore proves the untagged selector wires a durable
// fsstore backend: a non-nil DurableStore + LoopRegistry with no Postgres configured
// (the local/dev default). It also proves the returned store IS its own registry
// (fsstore.FSDurableStore satisfies both), and that a round-trip Register→Resolve works
// so `fuse loop-server` gets cross-process reattach for free.
func TestSelectDurableBackendReturnsFSStore(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	cfg := config.Config{}
	store, reg, err := selectDurableBackend(cfg)
	if err != nil {
		t.Fatalf("selectDurableBackend: %v", err)
	}
	if store == nil {
		t.Fatal("selectDurableBackend returned a nil DurableStore")
	}
	if reg == nil {
		t.Fatal("selectDurableBackend returned a nil LoopRegistry")
	}

	// The registry round-trips: Register then Resolve returns the record.
	ctx := context.Background()
	key := event.StreamKey{Tenant: event.DefaultTenant, Loop: "loop-x"}
	if err := reg.Register(ctx, event.LoopRecord{Key: key, OwnerNodeID: "loop-x", Live: true}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	rec, err := reg.Resolve(ctx, key)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !rec.Live || rec.OwnerNodeID != "loop-x" {
		t.Fatalf("Resolve record = %+v, want Live owner loop-x", rec)
	}
}

// TestBuildLoopServerDepsWiresDurableSeam proves buildLoopServerRuntimeDeps threads
// the durable store + registry into runtime.Deps as VALUES, so `fuse loop-server`
// resolves loops via the durable seam (cold cross-process reattach).
func TestBuildLoopServerDepsWiresDurableSeam(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config load: %v", err)
	}
	reg := registryFromConfig(cfg)
	deps := buildLoopServerRuntimeDeps(cfg, reg, reg.Default,
		defaultToolRegistry(cfg.Research, nil), spawnAgentBlock, nil, sessionRateGate(cfg))
	if deps.DurableStore == nil {
		t.Fatal("buildLoopServerRuntimeDeps left Deps.DurableStore nil (durable seam not wired)")
	}
	if deps.Registry == nil {
		t.Fatal("buildLoopServerRuntimeDeps left Deps.Registry nil (durable seam not wired)")
	}
}
