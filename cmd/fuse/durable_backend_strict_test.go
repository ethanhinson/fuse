package main

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/event"
)

// TestLoopServeNetRefusesWhenDurableBackendFails pins the fix for a defect found
// running the #0076 Helm chart live: the server and its Postgres started
// concurrently, the first server pod lost the race, and the composition root
// ABSORBED the selector error — the pod ran on the per-loop filesystem store,
// reported Ready (a nil store is "nothing to probe"), accepted loops, and every one
// of them was gone after the next rollout. A server binding must refuse to start
// instead: under Kubernetes that is a restart until the database is up.
func TestLoopServeNetRefusesWhenDurableBackendFails(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	oldSelect := selectDurableBackendFn
	selectDurableBackendFn = func(config.Config) (event.DurableStore, event.LoopRegistry, error) {
		return nil, nil, errors.New("pgstore: open: connection refused")
	}
	defer func() { selectDurableBackendFn = oldSelect }()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	oldListen := netListen
	netListen = func(string, string) (net.Listener, error) { return ln, nil }
	defer func() { netListen = oldListen }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	oldCtx := serveNetContext
	serveNetContext = func() (context.Context, context.CancelFunc) { return ctx, func() {} }
	defer func() { serveNetContext = oldCtx }()

	var out, errb strings.Builder
	code := run([]string{"loop-serve-net"}, &out, &errb)
	if code != 1 {
		t.Fatalf("loop-serve-net exit = %d, want 1 (refuse to start on a failed durable backend); stderr:\n%s", code, errb.String())
	}
	if !strings.Contains(errb.String(), "refusing to start on the filesystem store") {
		t.Fatalf("stderr does not name the refusal:\n%s", errb.String())
	}
	if !strings.Contains(errb.String(), "connection refused") {
		t.Fatalf("stderr does not carry the selector's cause:\n%s", errb.String())
	}
}

// TestLenientBuilderStillFallsBack proves the lenient variant tests and library
// callers use keeps its pre-existing behaviour: a selector error degrades to the
// nil store/registry (legacy per-loop fsstore) rather than panicking or erroring.
func TestLenientBuilderStillFallsBack(t *testing.T) {
	oldSelect := selectDurableBackendFn
	selectDurableBackendFn = func(config.Config) (event.DurableStore, event.LoopRegistry, error) {
		return nil, nil, errors.New("boom")
	}
	defer func() { selectDurableBackendFn = oldSelect }()

	t.Setenv("HOME", t.TempDir())
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config load: %v", err)
	}
	reg := registryFromConfig(cfg)
	deps := buildLoopServerRuntimeDeps(nil, cfg, reg, reg.Default, defaultToolRegistry(nil, cfg.Research, nil), "", nil, nil)
	if deps.DurableStore != nil || deps.Registry != nil {
		t.Fatal("lenient builder must fall back to nil store/registry on a selector error")
	}
	if _, err := buildLoopServerRuntimeDepsStrict(nil, cfg, reg, reg.Default, defaultToolRegistry(nil, cfg.Research, nil), "", nil, nil, nil); err == nil {
		t.Fatal("strict builder must return the selector error")
	}
}
