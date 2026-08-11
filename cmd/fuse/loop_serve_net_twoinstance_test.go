package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/event/fsstore"
	"github.com/ethanhinson/fuse/internal/loopconnect"
	loopv1 "github.com/ethanhinson/fuse/internal/loopwire/v1"
	"github.com/ethanhinson/fuse/internal/loopwire/v1/loopv1connect"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/runtime"
)

// TestTwoInstanceDifferentInstanceReconnect is the change-0055 ACCEPTANCE test
// (plan Task 5): TWO fully independent connect-go servers (two fully-wired runtimes —
// deglobalize-holder-also-per-instance-the-shared-graph, NOT a shared runtime graph)
// over ONE durable store (fsstore.NewDurableFSStore; no Postgres/testcontainers
// needed). A loop is started on instance A and driven through one turn via the
// scripted LLM_GATEWAY_URL double; the client Observes on A, then RE-OPENS
// Observe(from_seq) routed onto instance B and asserts B replays A's loop with no
// loss and no duplication. A concurrent append is forced into B's subscribe→replay
// window (via a direct store Append on the loop's StreamKey) to keep the dedup honest
// (replay-live-handoff-dedup-at-watermark).
//
// Store used: fsstore (filesystem durable store). Postgres/testcontainers is NOT
// required — the fsstore cross-instance path (durable registry + per-StreamKey
// subscribe) fully satisfies the different-instance reconnect requirement.
func TestTwoInstanceDifferentInstanceReconnect(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	// Scripted gateway double: echoes a fixed marker reply so the loop produces a
	// deterministic, model-free turn. NEVER a real provider (project policy).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"REPLY-ACCEPTANCE"}}]}`)
	}))
	defer srv.Close()
	t.Setenv("LLM_GATEWAY_URL", srv.URL)
	t.Setenv("LLM_GATEWAY_KEY", "tkn")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config load: %v", err)
	}
	reg := registryFromConfig(cfg)
	alias := reg.Default

	// ONE shared durable store + registry (fsstore) under a single temp dir. BOTH
	// runtimes below key their per-loop streams into THIS store, so a loop A starts is
	// resolvable/replayable from B (cross-instance).
	dir := t.TempDir()
	store := fsstore.NewDurableFSStore(dir)

	// Build a runtime's Deps from the SAME production wiring binding #2/#3 use, then
	// override the durable seam to the shared store and clear BaseDir so ONLY the
	// durable path is exercised (no legacy per-loop fsstore). This is called TWICE to
	// produce two INDEPENDENT runtime graphs over the one store.
	buildDeps := func() runtime.Deps {
		d := buildLoopServerRuntimeDeps(cfg, reg, alias, defaultToolRegistry(cfg.Research, nil),
			spawnAgentBlock, permissions.AlwaysApprove, sessionRateGate(cfg))
		d.DurableStore = store
		d.Registry = store
		d.BaseDir = "" // durable-only; no legacy fallback.
		return d
	}
	rtA := runtime.New(buildDeps())
	rtB := runtime.New(buildDeps())

	// Stand up two connect-go servers, one per runtime.
	serverFor := func(rt runtime.Runtime) (loopv1connect.LoopServiceClient, func()) {
		_, h := loopv1connect.NewLoopServiceHandler(loopconnect.NewHandler(rt).WithKeepalive(time.Hour))
		s := httptest.NewServer(h)
		return loopv1connect.NewLoopServiceClient(s.Client(), s.URL), s.Close
	}
	clientA, closeA := serverFor(rtA)
	defer closeA()
	clientB, closeB := serverFor(rtB)
	defer closeB()

	ctx := context.Background()

	// StartLoop on A and drive its single turn to completion via the gateway double.
	startResp, err := clientA.StartLoop(ctx, connect.NewRequest(&loopv1.StartLoopRequest{
		Task: "do the acceptance thing", Model: alias,
	}))
	if err != nil {
		t.Fatalf("A StartLoop: %v", err)
	}
	loopID := startResp.Msg.LoopId
	if loopID == "" {
		t.Fatal("A StartLoop returned empty loop_id")
	}

	// Wait for A's one-shot loop to fully finish (turn.end persisted) by polling the
	// durable store directly, so the durable history is complete before we observe.
	// This avoids racing the loop's final flush.
	lastSeqA := waitForTurnEnd(t, rtA, loopID)

	// Observe on A from 0: it must replay the full durable history now that the loop
	// is done. Record the ordered seqs seen.
	seenA := observeCollectThrough(t, clientA, loopID, 0, lastSeqA)
	if len(seenA) == 0 {
		t.Fatal("A Observe saw no events")
	}
	assertContiguous(t, seenA, "instance A")

	// Now RE-OPEN Observe(from_seq) routed onto instance B, from an EARLIER seq than A
	// saw (from 0) so B must replay A's full durable history — proving cross-instance
	// reconnect. Concurrently force an append into B's subscribe→replay window via a
	// direct store Append on the loop's StreamKey; the handler's subscribe-before-
	// replay + watermark dedup must deliver it EXACTLY once.
	key := event.StreamKey{Tenant: event.DefaultTenant, Loop: event.LoopID(loopID)}
	forcedSeq := lastSeqA + 1

	var wg sync.WaitGroup
	wg.Add(1)
	appendDone := make(chan struct{})
	go func() {
		defer wg.Done()
		// A tiny stagger so the append tends to land during B's observe setup; the dedup
		// contract holds regardless of exact interleaving.
		time.Sleep(20 * time.Millisecond)
		_ = store.Append(context.Background(), key, event.Event{Kind: event.KindError})
		close(appendDone)
	}()

	// Observe on B from 0; collect until we've seen the forced post-history event
	// (seq forcedSeq) OR a generous timeout. Assert exactly-once for every seq.
	seenB := observeCollectThrough(t, clientB, loopID, 0, forcedSeq)
	wg.Wait()
	<-appendDone

	assertContiguous(t, seenB, "instance B (cross-instance replay)")

	// B must have replayed at least A's full history plus the forced event.
	if len(seenB) < len(seenA) {
		t.Fatalf("B replayed %d events, want >= A's %d (loss on cross-instance reconnect)", len(seenB), len(seenA))
	}
	if seenB[len(seenB)-1] != forcedSeq {
		t.Fatalf("B last seq = %d, want the forced append seq %d", seenB[len(seenB)-1], forcedSeq)
	}
}

// waitForTurnEnd polls the runtime's durable Attach until the loop's history
// contains a turn.end event (the one-shot loop has finished and its final event is
// persisted), returning the last seq. It resolves the durable-store flush race so the
// subsequent Observe replays a complete history.
func waitForTurnEnd(t *testing.T, rt runtime.Runtime, loopID string) uint64 {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("timeout waiting for loop turn.end to persist")
		}
		hist, err := rt.Attach(context.Background(), event.DefaultTenant, loopID, 0)
		if err == nil {
			for _, e := range hist {
				if e.Kind == event.KindTurnEnd {
					return uint64(hist[len(hist)-1].Seq)
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// observeCollectThrough opens Observe(from) and collects ordered seqs until it sees
// `through`, returning them.
func observeCollectThrough(t *testing.T, client loopv1connect.LoopServiceClient, loopID string, from, through uint64) []uint64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	stream, err := client.Observe(ctx, connect.NewRequest(&loopv1.ObserveRequest{LoopId: loopID, FromSeq: from}))
	if err != nil {
		t.Fatalf("Observe(collect): %v", err)
	}
	var seqs []uint64
	for stream.Receive() {
		msg := stream.Msg()
		if msg.Keepalive || msg.Event == nil {
			continue
		}
		seqs = append(seqs, msg.Event.Seq)
		if msg.Event.Seq >= through {
			return seqs
		}
	}
	t.Fatalf("Observe ended before seq %d (err=%v); saw %v", through, stream.Err(), seqs)
	return seqs
}

// assertContiguous asserts the seqs are strictly increasing with NO duplicates (the
// exactly-once contract). It does not require starting at 1 (a from_seq > 0 replay
// starts later) but forbids repeats and out-of-order delivery.
func assertContiguous(t *testing.T, seqs []uint64, who string) {
	t.Helper()
	seen := map[uint64]bool{}
	var prev uint64
	for i, s := range seqs {
		if seen[s] {
			t.Fatalf("%s: seq %d delivered twice (double-delivery) in %v", who, s, seqs)
		}
		seen[s] = true
		if i > 0 && s <= prev {
			t.Fatalf("%s: seq %d not strictly increasing after %d in %v", who, s, prev, seqs)
		}
		prev = s
	}
}
