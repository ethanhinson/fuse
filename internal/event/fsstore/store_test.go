package fsstore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/event"
)

func newStore(t *testing.T) (*FSEventStore, string) {
	t.Helper()
	base := t.TempDir()
	s, err := NewFSEventStore(base, "sess-1")
	if err != nil {
		t.Fatalf("NewFSEventStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, base
}

// TestAppendReplayRoundTrip: Append then Replay(0) returns the same events in Seq
// order with byte-exact payloads.
func TestAppendReplayRoundTrip(t *testing.T) {
	s, base := newStore(t)
	pl, _ := event.MarshalPayload(event.ToolResultPayload{ID: "c1", Name: "bash", Result: "line1\nkey: v\n"})
	in := []event.Event{
		{Kind: event.KindTurnStart, Turn: 0},
		{Kind: event.KindToolResult, Turn: 0, Payload: pl},
		{Kind: event.KindTurnEnd, Turn: 0},
	}
	for _, e := range in {
		if err := s.Append(e); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	// The file must live at <base>/<sessionID>/events.jsonl.
	if _, err := os.Stat(filepath.Join(base, "sess-1", "events.jsonl")); err != nil {
		t.Fatalf("events.jsonl not at expected path: %v", err)
	}
	got, err := s.Replay(0)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("Replay returned %d events, want 3", len(got))
	}
	for i, e := range got {
		if e.Seq != event.Seq(i+1) {
			t.Errorf("event %d Seq = %d, want %d (store-allocated, monotonic from 1)", i, e.Seq, i+1)
		}
		if e.Kind != in[i].Kind {
			t.Errorf("event %d Kind = %q, want %q", i, e.Kind, in[i].Kind)
		}
	}
	// Payload byte-exactness on the nasty one.
	var rp event.ToolResultPayload
	if err := json.Unmarshal(got[1].Payload, &rp); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	if rp.Result != "line1\nkey: v\n" {
		t.Errorf("payload corrupted: %q", rp.Result)
	}
}

// TestReplayFromCursor: Replay(from) returns only Seq > from.
func TestReplayFromCursor(t *testing.T) {
	s, _ := newStore(t)
	for i := 0; i < 5; i++ {
		if err := s.Append(event.Event{Kind: event.KindTurnStart, Turn: i}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	got, err := s.Replay(3)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Replay(3) returned %d, want 2", len(got))
	}
	if got[0].Seq != 4 || got[1].Seq != 5 {
		t.Errorf("Replay(3) seqs = %d,%d want 4,5", got[0].Seq, got[1].Seq)
	}
}

// TestSeqIsStoreAllocated: a caller-supplied Seq is ignored; the store assigns a
// monotonic Seq under its lock.
func TestSeqIsStoreAllocated(t *testing.T) {
	s, _ := newStore(t)
	if err := s.Append(event.Event{Seq: 999, Kind: event.KindError}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	got, _ := s.Replay(0)
	if len(got) != 1 || got[0].Seq != 1 {
		t.Fatalf("caller Seq not overridden: %+v", got)
	}
}

// TestSubscribeLiveTail: a subscriber taken before appends receives the live
// stream; unsubscribe stops delivery and is idempotent.
func TestSubscribeLiveTail(t *testing.T) {
	s, _ := newStore(t)
	ch, cancel := s.Subscribe()
	if err := s.Append(event.Event{Kind: event.KindTurnStart}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	select {
	case e := <-ch:
		if e.Kind != event.KindTurnStart || e.Seq != 1 {
			t.Errorf("live event mismatch: %+v", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber did not receive the live event")
	}
	cancel()
	cancel() // idempotent
	// After cancel, a further append must not panic (delivery to a removed sub is a no-op).
	if err := s.Append(event.Event{Kind: event.KindTurnEnd}); err != nil {
		t.Fatalf("Append after cancel: %v", err)
	}
}

// TestLazyDirCreation: the session dir is created lazily by NewFSEventStore/Append,
// not required to pre-exist.
func TestLazyDirCreation(t *testing.T) {
	base := t.TempDir()
	// point at a nested, not-yet-existing base subdir
	nested := filepath.Join(base, "does", "not", "exist")
	s, err := NewFSEventStore(nested, "sess-x")
	if err != nil {
		t.Fatalf("NewFSEventStore on missing dir: %v", err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Append(event.Event{Kind: event.KindTurnStart}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if _, err := os.Stat(filepath.Join(nested, "sess-x", "events.jsonl")); err != nil {
		t.Fatalf("lazy dir/file not created: %v", err)
	}
}

// TestNonBlockingSlowSubscriber is the ADR-0016 regression guard: a subscriber
// whose buffer is full must NEVER block Append (which the loop calls inline). We
// fill the subscriber's buffer, then time a burst of Appends and assert they all
// return promptly, and that a gap was recorded.
func TestNonBlockingSlowSubscriber(t *testing.T) {
	s, _ := newStore(t)
	_, cancel := s.Subscribe() // a subscriber that NEVER reads
	defer cancel()

	// Append many more than the buffer can hold; a blocking send would hang here.
	n := subBuffer + 200
	done := make(chan struct{})
	go func() {
		for i := 0; i < n; i++ {
			_ = s.Append(event.Event{Kind: event.KindTurnStart, Turn: i})
		}
		close(done)
	}()
	select {
	case <-done:
		// good — never blocked
	case <-time.After(5 * time.Second):
		t.Fatal("Append blocked on a full/idle subscriber (ADR-0016 violation)")
	}
	if s.Dropped() == 0 {
		t.Error("expected a drop/gap to be recorded for the idle subscriber")
	}
	// Durability is unaffected: every appended event is on disk regardless of the
	// stalled subscriber.
	got, err := s.Replay(0)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(got) != n {
		t.Errorf("Replay after burst = %d events, want %d (disk durability independent of subscribers)", len(got), n)
	}
}

// TestConcurrentAppendNoCorruption: N goroutines Append concurrently; the file has
// N well-formed lines with monotone unique Seqs (mutex guard). Run under -race.
func TestConcurrentAppendNoCorruption(t *testing.T) {
	s, _ := newStore(t)
	const g = 50
	var wg sync.WaitGroup
	wg.Add(g)
	for i := 0; i < g; i++ {
		go func(i int) {
			defer wg.Done()
			_ = s.Append(event.Event{Kind: event.KindToolCall, Turn: i})
		}(i)
	}
	wg.Wait()
	got, err := s.Replay(0)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(got) != g {
		t.Fatalf("Replay = %d, want %d", len(got), g)
	}
	seen := map[event.Seq]bool{}
	for _, e := range got {
		if e.Seq < 1 || e.Seq > g {
			t.Errorf("Seq out of range: %d", e.Seq)
		}
		if seen[e.Seq] {
			t.Errorf("duplicate Seq: %d", e.Seq)
		}
		seen[e.Seq] = true
	}
}
