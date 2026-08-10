package loopserver

import (
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/event/fsstore"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/runtime"
)

// storeBackedRuntime is a runtime.Runtime test double backed by a REAL
// event.EventStore (an fsstore under a temp dir), so Attach honors Replay(from)
// (Seq > from) and Observe returns a live Subscribe channel — exactly the primitives
// the reattach handoff exercises. StartLoop/Send/Spawn are unused by the observe
// path and return zero values.
type storeBackedRuntime struct {
	loopID string
	store  event.EventStore
}

func (r *storeBackedRuntime) StartLoop(ctx context.Context, cfg runtime.LoopConfig) (runtime.LoopHandle, error) {
	return storeHandle{id: r.loopID}, nil
}
func (r *storeBackedRuntime) Send(ctx context.Context, loopID, input string) error { return nil }
func (r *storeBackedRuntime) Spawn(ctx context.Context, loopID string, opts runtime.SpawnOpts) (runtime.SpawnHandle, error) {
	return nil, nil
}
func (r *storeBackedRuntime) Observe(loopID string) (<-chan event.Event, func(), error) {
	ch, cancel := r.store.Subscribe()
	return ch, cancel, nil
}
func (r *storeBackedRuntime) Attach(loopID string, from event.Seq) ([]event.Event, error) {
	return r.store.Replay(from)
}

type storeHandle struct{ id string }

func (h storeHandle) ID() string                     { return h.id }
func (h storeHandle) Wait() ([]model.Message, error) { return nil, nil }

// TestReattachReplaysThenResumesExactlyOnce proves the reattach contract: after an
// observer disconnects mid-stream, re-issuing loop.observe with from_seq=lastSeenSeq
// delivers every event with Seq in (lastSeen, current] exactly once via replay, then
// live resumes — no duplicate of an already-seen Seq and no gap in the replayed
// range. The observe client is a RAW json.Decoder over io.Pipe, NOT fuse's mcp
// client (learning mcp-read-pumps-drop-inbound-notifications).
func TestReattachReplaysThenResumesExactlyOnce(t *testing.T) {
	store, err := fsstore.NewFSEventStore(t.TempDir(), "loop-reattach")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	rt := &storeBackedRuntime{loopID: "loop-reattach", store: store}

	// Seed durable history Seq 1..3 before the first observe.
	appendKinds(t, store, []event.Kind{event.KindTurnStart, event.KindModelCallStart, event.KindModelCallEnd})

	// --- First observe from:0 — read the response + the K=3 replayed notes, then
	//     "disconnect" (stop reading / cancel the context). ---
	seen := map[event.Seq]int{}
	last1 := firstObserveReadK(t, rt, 0, 3, seen)
	if last1 != 3 {
		t.Fatalf("first observe last-seen seq = %d, want 3", last1)
	}

	// Store keeps appending while the observer is gone: Seq 4,5.
	appendKinds(t, store, []event.Kind{event.KindTurnStart, event.KindTurnEnd})

	// --- Re-observe from:lastSeen(3) — replay must deliver Seq 4,5 exactly once and
	//     no already-seen Seq (1,2,3) again. ---
	sr, sw := io.Pipe()
	cr, cw := io.Pipe()
	s := NewServer(cr, sw, rt)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Serve(ctx) }()
	go func() {
		_ = json.NewEncoder(cw).Encode(req{JSONRPC: "2.0", ID: json.RawMessage(`21`),
			Method: "loop.observe", Params: json.RawMessage(`{"loop_id":"loop-reattach","from_seq":3}`)})
	}()
	dec := json.NewDecoder(sr)

	// Response first: replayed must be exactly the (3,current] range = {4,5}.
	var f0 rawFrame
	if err := dec.Decode(&f0); err != nil {
		t.Fatalf("decode reattach response: %v", err)
	}
	if len(f0.ID) == 0 || f0.Error != nil {
		t.Fatalf("reattach frame 0 not a success response: %+v", f0)
	}
	var res observeResult
	if err := json.Unmarshal(f0.Result, &res); err != nil {
		t.Fatalf("unmarshal observeResult: %v", err)
	}
	if res.Replayed != 2 || res.LastSeq != 5 {
		t.Fatalf("reattach observeResult = %+v, want replayed:2 last_seq:5", res)
	}

	// Two replay notes: Seq 4 then 5, each exactly once, none previously seen.
	for _, wantSeq := range []event.Seq{4, 5} {
		np := readNote(t, dec)
		if np.Event.Seq != wantSeq {
			t.Fatalf("reattach replay seq = %d, want %d", np.Event.Seq, wantSeq)
		}
		seen[np.Event.Seq]++
	}

	// No Seq delivered twice across the two observe sessions.
	for seq, n := range seen {
		if n != 1 {
			t.Fatalf("Seq %d delivered %d times, want exactly once (no dup, no gap)", seq, n)
		}
	}
	// The full contiguous range 1..5 was covered exactly once.
	for seq := event.Seq(1); seq <= 5; seq++ {
		if seen[seq] != 1 {
			t.Fatalf("Seq %d missing from combined delivery (gap)", seq)
		}
	}

	// Live resumes after reattach: append Seq 6 and read its note.
	appendKinds(t, store, []event.Kind{event.KindTurnEnd})
	np := readNote(t, dec)
	if np.Event.Seq != 6 {
		t.Fatalf("post-reattach live seq = %d, want 6", np.Event.Seq)
	}
}

// firstObserveReadK issues loop.observe(from) on a fresh server, reads the response
// then exactly k replay notes, recording each seen Seq, then cancels (disconnect).
// It returns the highest Seq seen.
func firstObserveReadK(t *testing.T, rt runtime.Runtime, from event.Seq, k int, seen map[event.Seq]int) event.Seq {
	t.Helper()
	sr, sw := io.Pipe()
	cr, cw := io.Pipe()
	s := NewServer(cr, sw, rt)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // disconnect: the pump goroutine stops on ctx.Done.

	go func() { _ = s.Serve(ctx) }()
	go func() {
		_ = json.NewEncoder(cw).Encode(req{JSONRPC: "2.0", ID: json.RawMessage(`20`),
			Method: "loop.observe", Params: json.RawMessage(`{"loop_id":"loop-reattach","from_seq":` + itoa(from) + `}`)})
	}()
	dec := json.NewDecoder(sr)

	var f0 rawFrame
	if err := dec.Decode(&f0); err != nil {
		t.Fatalf("decode first observe response: %v", err)
	}
	var last event.Seq
	for i := 0; i < k; i++ {
		np := readNote(t, dec)
		seen[np.Event.Seq]++
		last = np.Event.Seq
	}
	return last
}

// appendKinds appends one event per kind to the store, letting the store assign Seq.
func appendKinds(t *testing.T, store event.EventStore, kinds []event.Kind) {
	t.Helper()
	for _, k := range kinds {
		if err := store.Append(event.Event{Kind: k}); err != nil {
			t.Fatalf("append %s: %v", k, err)
		}
	}
}

// itoa is a tiny uint→string for building the from_seq JSON literal.
func itoa(s event.Seq) string {
	if s == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for s > 0 {
		i--
		buf[i] = byte('0' + s%10)
		s /= 10
	}
	return string(buf[i:])
}
