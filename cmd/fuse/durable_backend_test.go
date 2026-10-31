package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/permissions"
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
	deps := buildLoopServerRuntimeDeps(nil, cfg, reg, reg.Default,
		defaultToolRegistry(nil, cfg.Research, nil), spawnAgentBlock, permissions.Approver{}, sessionRateGate(cfg))
	if deps.DurableStore == nil {
		t.Fatal("buildLoopServerRuntimeDeps left Deps.DurableStore nil (durable seam not wired)")
	}
	if deps.Registry == nil {
		t.Fatal("buildLoopServerRuntimeDeps left Deps.Registry nil (durable seam not wired)")
	}
}

// TestFSDurableBackendIsReadyOnAFreshHost guards first start: the filesystem store
// only created its sessions directory on the first event, while /readyz pings that
// directory, so a fresh host reported not-ready forever - a load balancer never sent
// it the first request that would have created the directory.
func TestFSDurableBackendIsReadyOnAFreshHost(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store, _, err := selectDurableBackend(config.Config{})
	if err != nil {
		t.Fatalf("selectDurableBackend: %v", err)
	}
	p, ok := store.(event.Pinger)
	if !ok {
		t.Fatal("the filesystem store is not a Pinger")
	}
	if err := p.Ping(context.Background()); err != nil {
		t.Fatalf("a fresh host is not ready: %v", err)
	}
}

// TestStrictBuilderNamesTheFilesystemRemedy: a sessions directory that cannot
// be created is reported as a filesystem problem, not as a Postgres one.
func TestStrictBuilderNamesTheFilesystemRemedy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// A regular file where ~/.fuse must be a directory makes MkdirAll fail.
	if err := os.WriteFile(filepath.Join(home, ".fuse"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	reg := registryFromConfig(cfg)
	_, err := buildLoopServerRuntimeDepsStrict(nil, cfg, reg, reg.Default, defaultToolRegistry(nil, cfg.Research, nil), "", permissions.Approver{}, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "can be created and written") || strings.Contains(err.Error(), "Postgres") {
		t.Fatalf("err = %v", err)
	}
}
