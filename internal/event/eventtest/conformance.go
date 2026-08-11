// Package eventtest holds the ONE shared behavioral conformance suite for the
// durable event-store seam (change 0047). Both backends — internal/event/fsstore
// (always) and internal/event/pgstore (under the pgstore build tag) — feed their
// own real constructor into RunDurableStoreConformance, so a single set of
// invariant checks proves parity (parity-test-feeds-each-side-its-own-production-
// source). It is a normal (non-_test.go) package so both backend test packages can
// import it; it imports only internal/event + testing.
package eventtest

import (
	"context"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/event"
)

// dropCounter is optionally implemented by a store to expose its non-blocking-send
// drop count, so the conformance suite can assert a recorded gap in the
// non-blocking case without knowing the concrete type.
type dropCounter interface {
	Dropped() uint64
}

// RunDurableStoreConformance runs the shared behavioral suite against a backend.
// newStore returns a fresh, empty DurableStore + LoopRegistry pair backed by the
// same underlying storage (they may be the same value). Each subtest calls newStore
// for isolation.
func RunDurableStoreConformance(t *testing.T, newStore func(t *testing.T) (event.DurableStore, event.LoopRegistry)) {
	t.Helper()

	t.Run("AppendAllocatesContiguousPerLoopSeqFrom1", func(t *testing.T) {
		store, _ := newStore(t)
		ctx := context.Background()
		key := event.StreamKey{Tenant: "acme", Loop: "L1"}
		for i := 0; i < 4; i++ {
			if err := store.Append(ctx, key, event.Event{Kind: event.KindTurnStart, Turn: i}); err != nil {
				t.Fatalf("Append %d: %v", i, err)
			}
		}
		got, err := store.Replay(ctx, key, 0)
		if err != nil {
			t.Fatalf("Replay: %v", err)
		}
		if len(got) != 4 {
			t.Fatalf("Replay len = %d, want 4", len(got))
		}
		for i, e := range got {
			if e.Seq != event.Seq(i+1) {
				t.Fatalf("event %d Seq = %d, want %d (contiguous per-loop from 1)", i, e.Seq, i+1)
			}
		}
	})

	t.Run("ReplayFromCursorTenantScoped", func(t *testing.T) {
		store, _ := newStore(t)
		ctx := context.Background()
		key := event.StreamKey{Tenant: "acme", Loop: "L1"}
		for i := 0; i < 5; i++ {
			if err := store.Append(ctx, key, event.Event{Kind: event.KindTurnStart, Turn: i}); err != nil {
				t.Fatalf("Append: %v", err)
			}
		}
		got, err := store.Replay(ctx, key, 3)
		if err != nil {
			t.Fatalf("Replay(3): %v", err)
		}
		if len(got) != 2 || got[0].Seq != 4 || got[1].Seq != 5 {
			t.Fatalf("Replay(3) = %+v, want seqs 4,5", seqsOf(got))
		}
	})

	t.Run("TwoLoopsOneTenantIndependentSeq", func(t *testing.T) {
		store, _ := newStore(t)
		ctx := context.Background()
		k1 := event.StreamKey{Tenant: "acme", Loop: "L1"}
		k2 := event.StreamKey{Tenant: "acme", Loop: "L2"}
		if err := store.Append(ctx, k1, event.Event{Kind: event.KindTurnStart}); err != nil {
			t.Fatalf("Append k1: %v", err)
		}
		if err := store.Append(ctx, k1, event.Event{Kind: event.KindTurnEnd}); err != nil {
			t.Fatalf("Append k1: %v", err)
		}
		if err := store.Append(ctx, k2, event.Event{Kind: event.KindError}); err != nil {
			t.Fatalf("Append k2: %v", err)
		}
		g1, _ := store.Replay(ctx, k1, 0)
		g2, _ := store.Replay(ctx, k2, 0)
		if len(g1) != 2 || g1[0].Seq != 1 || g1[1].Seq != 2 {
			t.Fatalf("L1 seqs = %v, want 1,2", seqsOf(g1))
		}
		if len(g2) != 1 || g2[0].Seq != 1 {
			t.Fatalf("L2 seqs = %v, want 1 (independent stream)", seqsOf(g2))
		}
	})

	t.Run("CrossTenantIsolation", func(t *testing.T) {
		store, reg := newStore(t)
		ctx := context.Background()
		kx := event.StreamKey{Tenant: "tenantX", Loop: "shared-loop"}
		if err := store.Append(ctx, kx, event.Event{Kind: event.KindTurnStart}); err != nil {
			t.Fatalf("Append X: %v", err)
		}
		if err := reg.Register(ctx, event.LoopRecord{Key: kx, OwnerNodeID: "nodeX", Owner: "subjectX", Live: true}); err != nil {
			t.Fatalf("Register X: %v", err)
		}
		// Same loop id under a DIFFERENT tenant must not see X's stream or record.
		ky := event.StreamKey{Tenant: "tenantY", Loop: "shared-loop"}
		gy, err := store.Replay(ctx, ky, 0)
		if err != nil {
			t.Fatalf("Replay Y: %v", err)
		}
		if len(gy) != 0 {
			t.Fatalf("cross-tenant leak: tenantY replay returned %d events", len(gy))
		}
		if _, err := reg.Resolve(ctx, ky); err != event.ErrLoopUnknown {
			t.Fatalf("Resolve(tenantY, X's loop) err = %v, want ErrLoopUnknown", err)
		}
		// List for Y is empty; List for X has exactly the one loop.
		ly, _ := reg.List(ctx, "tenantY")
		if len(ly) != 0 {
			t.Fatalf("List(tenantY) = %d, want 0", len(ly))
		}
		lx, _ := reg.List(ctx, "tenantX")
		if len(lx) != 1 || lx[0].Key != kx {
			t.Fatalf("List(tenantX) = %+v, want the one X loop", lx)
		}
		// The new Owner field round-trips through the tenant-scoped List/Resolve.
		if lx[0].Owner != "subjectX" {
			t.Fatalf("List(tenantX) Owner = %q, want subjectX", lx[0].Owner)
		}
	})

	t.Run("RegisterOwnerRoundTrips", func(t *testing.T) {
		_, reg := newStore(t)
		ctx := context.Background()
		key := event.StreamKey{Tenant: "acme", Loop: "owned"}
		if err := reg.Register(ctx, event.LoopRecord{Key: key, OwnerNodeID: "nodeA", Owner: "alice", Live: true}); err != nil {
			t.Fatalf("Register: %v", err)
		}
		rec, err := reg.Resolve(ctx, key)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if rec.Owner != "alice" {
			t.Fatalf("Resolve Owner = %q, want alice", rec.Owner)
		}
	})

	t.Run("HeartbeatAdvancesLeaseLeavesOwnerAndLive", func(t *testing.T) {
		_, reg := newStore(t)
		ctx := context.Background()
		key := event.StreamKey{Tenant: "acme", Loop: "leased"}
		if err := reg.Register(ctx, event.LoopRecord{Key: key, OwnerNodeID: "nodeA", Owner: "alice", Live: true}); err != nil {
			t.Fatalf("Register: %v", err)
		}
		before, err := reg.Resolve(ctx, key)
		if err != nil {
			t.Fatalf("Resolve before: %v", err)
		}
		expiry := time.Now().UTC().Add(30 * time.Second).Truncate(time.Millisecond)
		if err := reg.Heartbeat(ctx, key, "nodeA", expiry); err != nil {
			t.Fatalf("Heartbeat: %v", err)
		}
		after, err := reg.Resolve(ctx, key)
		if err != nil {
			t.Fatalf("Resolve after: %v", err)
		}
		if !after.LeaseExpiry.Equal(expiry) {
			t.Fatalf("Heartbeat LeaseExpiry = %v, want %v", after.LeaseExpiry, expiry)
		}
		if after.UpdatedAt.Before(before.UpdatedAt) {
			t.Fatalf("Heartbeat regressed UpdatedAt: before=%v after=%v", before.UpdatedAt, after.UpdatedAt)
		}
		// Owner and Live are untouched by a heartbeat.
		if after.Owner != "alice" {
			t.Fatalf("Heartbeat changed Owner = %q, want alice", after.Owner)
		}
		if !after.Live {
			t.Fatalf("Heartbeat changed Live = %v, want true", after.Live)
		}
	})

	t.Run("HeartbeatUnknownKeyErrLoopUnknown", func(t *testing.T) {
		_, reg := newStore(t)
		ctx := context.Background()
		key := event.StreamKey{Tenant: "acme", Loop: "never-registered"}
		if err := reg.Heartbeat(ctx, key, "nodeA", time.Now().Add(time.Minute)); err != event.ErrLoopUnknown {
			t.Fatalf("Heartbeat on unknown key = %v, want ErrLoopUnknown", err)
		}
	})

	t.Run("SubscribeBeforeReplayDedupAtWatermark", func(t *testing.T) {
		store, _ := newStore(t)
		ctx := context.Background()
		key := event.StreamKey{Tenant: "acme", Loop: "reattach"}
		// Pre-existing history.
		for i := 0; i < 3; i++ {
			if err := store.Append(ctx, key, event.Event{Kind: event.KindTurnStart, Turn: i}); err != nil {
				t.Fatalf("Append pre %d: %v", i, err)
			}
		}
		// Subscribe FIRST (subscribe-before-replay), then append during the window,
		// then replay. Reassemble with dedup at the replay watermark and assert every
		// Seq appears exactly once with no gap.
		ch, cancel, err := store.Subscribe(ctx, key)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		defer cancel()

		// Append during the window (these arrive live AND may appear in replay).
		for i := 3; i < 6; i++ {
			if err := store.Append(ctx, key, event.Event{Kind: event.KindTurnStart, Turn: i}); err != nil {
				t.Fatalf("Append window %d: %v", i, err)
			}
		}

		hist, err := store.Replay(ctx, key, 0)
		if err != nil {
			t.Fatalf("Replay: %v", err)
		}
		watermark := event.Seq(0)
		seen := map[event.Seq]int{}
		for _, e := range hist {
			seen[e.Seq]++
			if e.Seq > watermark {
				watermark = e.Seq
			}
		}
		// Drain live events past the watermark (dedup: skip <= watermark).
		drain := time.After(2 * time.Second)
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
				t.Fatalf("Seq %d appeared %d times (want exactly once; no gap, no dup); reassembled=%v", s, seen[s], seen)
			}
		}
	})

	t.Run("NonBlockingAppendUnderStalledSubscriber", func(t *testing.T) {
		store, _ := newStore(t)
		ctx := context.Background()
		key := event.StreamKey{Tenant: "acme", Loop: "stall"}
		// A subscriber that never reads.
		_, cancel, err := store.Subscribe(ctx, key)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		defer cancel()

		const n = 512 + 200 // more than any reasonable per-sub buffer
		done := make(chan struct{})
		go func() {
			for i := 0; i < n; i++ {
				_ = store.Append(ctx, key, event.Event{Kind: event.KindTurnStart, Turn: i})
			}
			close(done)
		}()
		select {
		case <-done:
			// Never blocked — good.
		case <-time.After(10 * time.Second):
			t.Fatal("Append blocked on a stalled subscriber (non-blocking fan-out violation)")
		}
		// Durability is independent of the stalled subscriber.
		got, err := store.Replay(ctx, key, 0)
		if err != nil {
			t.Fatalf("Replay: %v", err)
		}
		if len(got) != n {
			t.Fatalf("Replay after burst = %d, want %d (durability independent of subscribers)", len(got), n)
		}
		// If the store exposes a drop counter, a gap must have been recorded.
		if dc, ok := store.(dropCounter); ok {
			if dc.Dropped() == 0 {
				t.Fatal("expected a recorded drop/gap for the stalled subscriber")
			}
		}
	})

	t.Run("RegistryRoundTripLivenessOwnership", func(t *testing.T) {
		_, reg := newStore(t)
		ctx := context.Background()
		key := event.StreamKey{Tenant: "acme", Loop: "reg1"}

		if _, err := reg.Resolve(ctx, key); err != event.ErrLoopUnknown {
			t.Fatalf("Resolve before Register = %v, want ErrLoopUnknown", err)
		}
		if err := reg.Register(ctx, event.LoopRecord{Key: key, OwnerNodeID: "nodeA", Live: true}); err != nil {
			t.Fatalf("Register: %v", err)
		}
		rec, err := reg.Resolve(ctx, key)
		if err != nil {
			t.Fatalf("Resolve after Register: %v", err)
		}
		if rec.Key != key || rec.OwnerNodeID != "nodeA" || !rec.Live {
			t.Fatalf("resolved record mismatch: %+v", rec)
		}
		if rec.CreatedAt.IsZero() || rec.UpdatedAt.IsZero() {
			t.Fatalf("Register must set CreatedAt/UpdatedAt: %+v", rec)
		}

		// Flip liveness + owner.
		if err := reg.SetLive(ctx, key, false, "nodeB"); err != nil {
			t.Fatalf("SetLive: %v", err)
		}
		rec2, err := reg.Resolve(ctx, key)
		if err != nil {
			t.Fatalf("Resolve after SetLive: %v", err)
		}
		if rec2.Live {
			t.Fatalf("SetLive(false) did not clear liveness: %+v", rec2)
		}
		if rec2.OwnerNodeID != "nodeB" {
			t.Fatalf("SetLive owner = %q, want nodeB", rec2.OwnerNodeID)
		}

		// List returns this tenant's loops.
		ls, err := reg.List(ctx, "acme")
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(ls) != 1 || ls[0].Key != key {
			t.Fatalf("List(acme) = %+v, want the one loop", ls)
		}
	})
}

func seqsOf(evs []event.Event) []event.Seq {
	out := make([]event.Seq, len(evs))
	for i, e := range evs {
		out[i] = e.Seq
	}
	return out
}
