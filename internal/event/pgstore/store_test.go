//go:build pgstore

package pgstore

// store_test.go is the tagged Postgres test suite for change 0047. It runs ONLY
// under `//go:build pgstore` and is SKIP-CLEAN: if no container runtime (Docker) is
// available it t.Skips rather than hard-failing, so even `go test -tags pgstore`
// on a Docker-less box is green.
//
// Coverage:
//   - the ONE shared behavioral conformance suite (eventtest.RunDurableStoreConformance),
//     proving pgstore has parity with fsstore on every durable-store invariant;
//   - cross-instance pub/sub: two Open handles on the SAME container, A appends /
//     B subscribes, no gap no dup, plus a from_seq reattach (subscribe-before-replay
//     + dedup-at-watermark) with a concurrent append forced into the handoff gap;
//   - non-blocking Append under Postgres: a parked subscriber + burst of appends,
//     asserting prompt return + a recorded gap;
//   - tenant isolation: cross-tenant Resolve/Replay returns ErrLoopUnknown/empty,
//     never another tenant's stream.

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/event/eventtest"
)

// pgHarness is the package-shared testcontainers Postgres, started once (lazily) and
// reused across tests. When the container runtime is unavailable, skipReason is set
// and every test t.Skips.
type pgHarness struct {
	dsn        string
	skipReason string
	container  *tcpostgres.PostgresContainer
}

var (
	harnessOnce sync.Once
	harness     pgHarness
)

// getHarness starts (once) a testcontainers Postgres and returns its DSN. If Docker
// is unavailable it returns a non-empty skip reason instead of failing — the whole
// tagged suite is skip-clean on a Docker-less box.
func getHarness() pgHarness {
	harnessOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()

		container, err := tcpostgres.Run(ctx,
			"postgres:16-alpine",
			tcpostgres.WithDatabase("fuse_test"),
			tcpostgres.WithUsername("fuse"),
			tcpostgres.WithPassword("fuse"),
			tcpostgres.BasicWaitStrategies(),
		)
		if err != nil {
			harness.skipReason = fmt.Sprintf("no container runtime: %v", err)
			return
		}
		dsn, err := container.ConnectionString(ctx, "sslmode=disable")
		if err != nil {
			_ = testcontainers.TerminateContainer(container)
			harness.skipReason = fmt.Sprintf("connection string: %v", err)
			return
		}
		harness.container = container
		harness.dsn = dsn
	})
	return harness
}

// dsnOrSkip returns the shared container DSN, skipping the test if Docker is absent.
func dsnOrSkip(t *testing.T) string {
	t.Helper()
	h := getHarness()
	if h.skipReason != "" {
		t.Skip(h.skipReason)
	}
	return h.dsn
}

// openClean opens a PGStore against the shared container and TRUNCATEs the tables
// so each caller gets a clean slate (the conformance suite's newStore contract).
// The store is closed at test cleanup.
func openClean(t *testing.T) *PGStore {
	t.Helper()
	dsn := dsnOrSkip(t)
	ctx := context.Background()
	s, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `TRUNCATE events, loops`); err != nil {
		_ = s.Close()
		t.Fatalf("truncate: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// openHandle opens an ADDITIONAL PGStore handle against the same container WITHOUT
// truncating (a second "instance" sharing the database for cross-instance tests).
func openHandle(t *testing.T) *PGStore {
	t.Helper()
	dsn := dsnOrSkip(t)
	s, err := Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("Open handle: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestPGConformance runs the shared durable-store conformance suite against pgstore.
func TestPGConformance(t *testing.T) {
	// Establish the container up front so the whole suite skips cleanly if absent.
	_ = dsnOrSkip(t)
	eventtest.RunDurableStoreConformance(t, func(t *testing.T) (event.DurableStore, event.LoopRegistry) {
		s := openClean(t)
		return s, s
	})
}

// TestPGCrossInstancePubSub proves two Open handles on the SAME container form one
// shared pub/sub: instance A appends, instance B (a distinct handle) subscribes and
// receives A's events with no gap and no dup.
func TestPGCrossInstancePubSub(t *testing.T) {
	a := openClean(t) // truncates
	b := openHandle(t)
	ctx := context.Background()
	key := event.StreamKey{Tenant: "acme", Loop: "xinst"}

	ch, cancel, err := b.Subscribe(ctx, key)
	if err != nil {
		t.Fatalf("B.Subscribe: %v", err)
	}
	defer cancel()

	const n = 10
	for i := 0; i < n; i++ {
		if err := a.Append(ctx, key, event.Event{Kind: event.KindTurnStart, Turn: i}); err != nil {
			t.Fatalf("A.Append %d: %v", i, err)
		}
	}

	seen := map[event.Seq]int{}
	deadline := time.After(30 * time.Second)
	for len(seen) < n {
		select {
		case e := <-ch:
			seen[e.Seq]++
		case <-deadline:
			t.Fatalf("cross-instance tail timed out; got %d/%d: %v", len(seen), n, seen)
		}
	}
	for s := event.Seq(1); s <= n; s++ {
		if seen[s] != 1 {
			t.Fatalf("Seq %d delivered %d times cross-instance (want exactly once; no gap no dup)", s, seen[s])
		}
	}
}

// TestPGReattachFromSeq proves a from_seq reattach across instances: B subscribes
// before replaying, A appends a concurrent event forced into the handoff gap, then B
// replays from a watermark and dedups — every Seq appears exactly once.
func TestPGReattachFromSeq(t *testing.T) {
	a := openClean(t)
	b := openHandle(t)
	ctx := context.Background()
	key := event.StreamKey{Tenant: "acme", Loop: "reattach"}

	// Pre-existing history on A.
	for i := 0; i < 3; i++ {
		if err := a.Append(ctx, key, event.Event{Kind: event.KindTurnStart, Turn: i}); err != nil {
			t.Fatalf("A.Append pre %d: %v", i, err)
		}
	}

	// B subscribes FIRST (subscribe-before-replay).
	ch, cancel, err := b.Subscribe(ctx, key)
	if err != nil {
		t.Fatalf("B.Subscribe: %v", err)
	}
	defer cancel()

	// A concurrent append forced into the handoff gap (between subscribe and replay).
	for i := 3; i < 6; i++ {
		if err := a.Append(ctx, key, event.Event{Kind: event.KindTurnStart, Turn: i}); err != nil {
			t.Fatalf("A.Append window %d: %v", i, err)
		}
	}

	// B replays from 0, establishing the watermark, then dedups live past it.
	hist, err := b.Replay(ctx, key, 0)
	if err != nil {
		t.Fatalf("B.Replay: %v", err)
	}
	watermark := event.Seq(0)
	seen := map[event.Seq]int{}
	for _, e := range hist {
		seen[e.Seq]++
		if e.Seq > watermark {
			watermark = e.Seq
		}
	}
	drain := time.After(30 * time.Second)
loop:
	for watermark < 6 {
		select {
		case e := <-ch:
			if e.Seq > watermark {
				seen[e.Seq]++
				watermark = e.Seq
			}
		case <-drain:
			break loop
		}
	}
	for s := event.Seq(1); s <= 6; s++ {
		if seen[s] != 1 {
			t.Fatalf("Seq %d appeared %d times on reattach (want exactly once; no gap no dup): %v", s, seen[s], seen)
		}
	}
}

// TestPGNonBlockingAppend proves Append never back-pressures on NOTIFY delivery or a
// parked subscriber under Postgres: a subscriber is parked (never reads), a burst of
// appends is issued, Append returns promptly and a gap is recorded.
func TestPGNonBlockingAppend(t *testing.T) {
	s := openClean(t)
	ctx := context.Background()
	key := event.StreamKey{Tenant: "acme", Loop: "stall"}

	// A parked subscriber that never reads.
	_, cancel, err := s.Subscribe(ctx, key)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cancel()

	const n = subBuffer + publishQueueDepth + 200 // exceed both bounded buffers
	done := make(chan struct{})
	go func() {
		for i := 0; i < n; i++ {
			_ = s.Append(ctx, key, event.Event{Kind: event.KindTurnStart, Turn: i})
		}
		close(done)
	}()
	select {
	case <-done:
		// Never blocked — good.
	case <-time.After(60 * time.Second):
		t.Fatal("Append blocked under a parked subscriber (non-blocking violation)")
	}

	// Durability is independent of the parked subscriber.
	got, err := s.Replay(ctx, key, 0)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(got) != n {
		t.Fatalf("Replay after burst = %d, want %d (durability independent of subscribers)", len(got), n)
	}
	// A gap must have been recorded (the parked subscriber and/or the notify queue
	// overflowed).
	if s.Dropped() == 0 {
		t.Fatal("expected a recorded drop/gap for the parked subscriber under burst")
	}
}

// TestPGTenantIsolation proves cross-tenant Resolve/Replay never returns another
// tenant's stream.
func TestPGTenantIsolation(t *testing.T) {
	s := openClean(t)
	ctx := context.Background()
	kx := event.StreamKey{Tenant: "tenantX", Loop: "shared-loop"}
	if err := s.Append(ctx, kx, event.Event{Kind: event.KindTurnStart}); err != nil {
		t.Fatalf("Append X: %v", err)
	}
	if err := s.Register(ctx, event.LoopRecord{Key: kx, OwnerNodeID: "nodeX", Live: true}); err != nil {
		t.Fatalf("Register X: %v", err)
	}

	ky := event.StreamKey{Tenant: "tenantY", Loop: "shared-loop"}
	gy, err := s.Replay(ctx, ky, 0)
	if err != nil {
		t.Fatalf("Replay Y: %v", err)
	}
	if len(gy) != 0 {
		t.Fatalf("cross-tenant leak: tenantY replay returned %d events", len(gy))
	}
	if _, err := s.Resolve(ctx, ky); err != event.ErrLoopUnknown {
		t.Fatalf("Resolve(tenantY) err = %v, want ErrLoopUnknown", err)
	}
	ly, _ := s.List(ctx, "tenantY")
	if len(ly) != 0 {
		t.Fatalf("List(tenantY) = %d, want 0", len(ly))
	}
}
