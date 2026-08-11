package loopconnect

import (
	"context"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/ethanhinson/fuse/internal/event"
	loopv1 "github.com/ethanhinson/fuse/internal/loopwire/v1"
	"github.com/ethanhinson/fuse/internal/loopwire/v1/loopv1connect"
)

// observeFake is a runtime.Runtime double built to exercise the subscribe→replay
// handoff: Observe hands back a live channel and records that a subscriber attached
// (so a test can force an append into the window), and Attach returns a scripted
// history. It also tracks unsubscribe calls to prove leak-free teardown.
type observeFake struct {
	fakeRuntime

	live       chan event.Event
	subscribed chan struct{} // closed once (via once) when Observe is called
	once       sync.Once
	unsubCount atomic.Int32
	hist       func() []event.Event
}

func newObserveFake() *observeFake {
	f := &observeFake{
		live:       make(chan event.Event, 32),
		subscribed: make(chan struct{}),
	}
	f.observeFn = func(ctx context.Context, tn event.TenantID, loopID string) (<-chan event.Event, func(), error) {
		f.once.Do(func() { close(f.subscribed) })
		return f.live, func() { f.unsubCount.Add(1) }, nil
	}
	f.attachFn = func(ctx context.Context, tn event.TenantID, loopID string, from event.Seq) ([]event.Event, error) {
		var out []event.Event
		for _, e := range f.hist() {
			if e.Seq > from {
				out = append(out, e)
			}
		}
		return out, nil
	}
	return f
}

// TestObserveReplayLiveHandoffDedup is THE load-bearing test
// (replay-live-handoff-dedup-at-watermark): it forces a concurrent append into the
// subscribe→replay window and asserts the client sees each Seq EXACTLY once — no
// double-delivery of the overlap event, and no loss.
func TestObserveReplayLiveHandoffDedup(t *testing.T) {
	f := newObserveFake()

	// Seq 3 is the overlap event: it is appended into the live channel BEFORE Attach
	// snapshots history, so it appears in BOTH the replay (history includes 1..3) AND
	// the live tail. The handler must deliver it once.
	var histMu sync.Mutex
	histEvents := []event.Event{{Seq: 1}, {Seq: 2}}
	f.hist = func() []event.Event {
		histMu.Lock()
		defer histMu.Unlock()
		out := make([]event.Event, len(histEvents))
		copy(out, histEvents)
		return out
	}

	// Wrap Observe so we inject the overlap event AFTER subscribe but BEFORE Attach
	// reads history: the handler subscribes, we push seq 3 into live and also add it to
	// history (so history is 1..3 when Attach snapshots), then let Attach proceed.
	origObserve := f.observeFn
	f.observeFn = func(ctx context.Context, tn event.TenantID, loopID string) (<-chan event.Event, func(), error) {
		ch, unsub, err := origObserve(ctx, tn, loopID)
		// Now force the overlap: seq 3 lands in the live buffer AND in history.
		f.live <- event.Event{Seq: 3}
		histMu.Lock()
		histEvents = append(histEvents, event.Event{Seq: 3})
		histMu.Unlock()
		return ch, unsub, err
	}

	client := newObserveTestServer(t, NewHandler(&f.fakeRuntime).WithKeepalive(time.Hour), f)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := client.Observe(ctx, connect.NewRequest(&loopv1.ObserveRequest{LoopId: "l", FromSeq: 0}))
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}

	// Push a couple more genuinely-new live events so the stream keeps flowing.
	f.live <- event.Event{Seq: 4}
	f.live <- event.Event{Seq: 5}

	// Collect the first 5 event frames (ignoring keepalives) and assert exact-once,
	// contiguous 1..5 with no duplicate of the overlap seq 3.
	seen := collectSeqs(t, stream, 5)
	want := []uint64{1, 2, 3, 4, 5}
	if len(seen) != len(want) {
		t.Fatalf("got seqs %v, want %v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("got seqs %v, want %v (double-delivery or loss)", seen, want)
		}
	}
	cancel()
}

// TestObserveGapFlaggedOnSequenceBreak asserts the first live event past a forced
// hole carries gap:true (ADR-0025 drop-newest re-observe hint).
func TestObserveGapFlaggedOnSequenceBreak(t *testing.T) {
	f := newObserveFake()
	f.hist = func() []event.Event { return []event.Event{{Seq: 1}, {Seq: 2}} }

	client := newObserveTestServer(t, NewHandler(&f.fakeRuntime).WithKeepalive(time.Hour), f)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := client.Observe(ctx, connect.NewRequest(&loopv1.ObserveRequest{LoopId: "l", FromSeq: 0}))
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	<-f.subscribed

	// Replay watermark is 2. Push seq 5 (a hole: 3,4 dropped) → gap must be true.
	f.live <- event.Event{Seq: 5}

	// Skip the two replayed frames (1,2 — gap false), then read the live seq-5 frame.
	skipEventFrames(t, stream, 2)
	fr := nextEventFrame(t, stream)
	if fr.Event.Seq != 5 {
		t.Fatalf("live seq = %d, want 5", fr.Event.Seq)
	}
	if !fr.Gap {
		t.Fatal("expected gap:true on the first event past a sequence hole")
	}
	// A contiguous follow-on event (6) must NOT be flagged.
	f.live <- event.Event{Seq: 6}
	fr2 := nextEventFrame(t, stream)
	if fr2.Event.Seq != 6 || fr2.Gap {
		t.Fatalf("seq 6 frame = seq:%d gap:%v, want seq:6 gap:false", fr2.Event.Seq, fr2.Gap)
	}
	cancel()
}

// TestObserveAbortedStreamTearsDownNoLeak asserts an aborted stream context releases
// the subscription (unsubscribe is invoked exactly once) — no leaked
// goroutine/subscription (defer cancel() on every return path).
func TestObserveAbortedStreamTearsDownNoLeak(t *testing.T) {
	f := newObserveFake()
	f.hist = func() []event.Event { return []event.Event{{Seq: 1}} }

	client := newObserveTestServer(t, NewHandler(&f.fakeRuntime).WithKeepalive(time.Hour), f)
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := client.Observe(ctx, connect.NewRequest(&loopv1.ObserveRequest{LoopId: "l", FromSeq: 0}))
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	<-f.subscribed
	// Read the replayed frame so the handler is live-tailing, then abort.
	skipEventFrames(t, stream, 1)
	cancel()
	_ = stream.Close()

	// The handler's deferred cancel() must fire, unsubscribing exactly once. Poll
	// briefly since teardown is async on the server side.
	deadline := time.After(5 * time.Second)
	for {
		if f.unsubCount.Load() >= 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("subscription not released after stream abort (leak)")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if got := f.unsubCount.Load(); got != 1 {
		t.Fatalf("unsubscribe called %d times, want exactly 1 (idempotent teardown)", got)
	}
}

// --- helpers ----------------------------------------------------------------------

func newObserveTestServer(t *testing.T, h *Handler, _ *observeFake) loopv1connect.LoopServiceClient {
	t.Helper()
	_, handler := loopv1connect.NewLoopServiceHandler(h)
	// Connect server-streaming works over HTTP/1.1 (chunked), so a plain httptest
	// server is sufficient for these unit tests — no h2c needed.
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return loopv1connect.NewLoopServiceClient(srv.Client(), srv.URL)
}

// collectSeqs reads n event-bearing frames (skipping keepalives), returning their
// seqs in order.
func collectSeqs(t *testing.T, stream *connect.ServerStreamForClient[loopv1.ObserveEvent], n int) []uint64 {
	t.Helper()
	var out []uint64
	deadline := time.Now().Add(10 * time.Second)
	for len(out) < n {
		if time.Now().After(deadline) {
			t.Fatalf("timeout collecting %d seqs; got %v", n, out)
		}
		if !stream.Receive() {
			t.Fatalf("stream ended early (err=%v); got %v", stream.Err(), out)
		}
		msg := stream.Msg()
		if msg.Keepalive || msg.Event == nil {
			continue
		}
		out = append(out, msg.Event.Seq)
	}
	return out
}

func skipEventFrames(t *testing.T, stream *connect.ServerStreamForClient[loopv1.ObserveEvent], n int) {
	t.Helper()
	got := 0
	for got < n {
		if !stream.Receive() {
			t.Fatalf("stream ended early skipping frames (err=%v)", stream.Err())
		}
		if stream.Msg().Keepalive || stream.Msg().Event == nil {
			continue
		}
		got++
	}
}

func nextEventFrame(t *testing.T, stream *connect.ServerStreamForClient[loopv1.ObserveEvent]) *loopv1.ObserveEvent {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("timeout awaiting next event frame")
		}
		if !stream.Receive() {
			t.Fatalf("stream ended early (err=%v)", stream.Err())
		}
		if stream.Msg().Keepalive || stream.Msg().Event == nil {
			continue
		}
		return stream.Msg()
	}
}
