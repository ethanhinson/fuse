package event

import (
	"testing"
	"time"
)

func TestMemStoreAllocatesSeqAndReplays(t *testing.T) {
	s := NewMemStore(0)
	for i := 0; i < 3; i++ {
		if err := s.Append(Event{Kind: KindTurnStart, Seq: 99}); err != nil { // caller Seq is ignored
			t.Fatal(err)
		}
	}
	all, _ := s.Replay(0)
	if len(all) != 3 || all[0].Seq != 1 || all[2].Seq != 3 {
		t.Fatalf("replay = %+v", all)
	}
	if all[0].TS.IsZero() {
		t.Error("TS not stamped")
	}
	tail, _ := s.Replay(2)
	if len(tail) != 1 || tail[0].Seq != 3 {
		t.Fatalf("replay(2) = %+v", tail)
	}
}

func TestMemStoreKeepsNewestWindowButSeqKeepsRising(t *testing.T) {
	s := NewMemStore(3)
	for i := 0; i < 10; i++ {
		_ = s.Append(Event{Kind: KindTurnEnd})
	}
	got, _ := s.Replay(0)
	if len(got) != 3 || got[0].Seq != 8 || got[2].Seq != 10 {
		t.Fatalf("window = %+v, want seq 8..10", got)
	}
}

func TestMemStoreSubscribeIsLiveAndNonBlocking(t *testing.T) {
	s := NewMemStore(0)
	ch, unsub := s.Subscribe()
	_ = s.Append(Event{Kind: KindToolCall})
	select {
	case e := <-ch:
		if e.Kind != KindToolCall || e.Seq != 1 {
			t.Fatalf("live event = %+v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("no live event")
	}

	// A subscriber that never reads must not stall Append.
	slow, slowUnsub := s.Subscribe()
	_ = slow
	done := make(chan struct{})
	go func() {
		for i := 0; i < memSubBuffer*3; i++ {
			_ = s.Append(Event{Kind: KindTurnStart})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Append blocked on a slow subscriber")
	}
	unsub()
	unsub() // idempotent
	slowUnsub()
}

func TestMemStoreCloseEndsSubscribersAndKeepsReplay(t *testing.T) {
	s := NewMemStore(0)
	ch, _ := s.Subscribe()
	_ = s.Append(Event{Kind: KindToolCall})
	_ = s.Close()
	_ = s.Close() // idempotent
	for range ch {
	} // drains then ends: the channel was closed
	if got, _ := s.Replay(0); len(got) != 1 {
		t.Fatalf("replay after close = %+v", got)
	}
	if err := s.Append(Event{Kind: KindToolCall}); err != nil {
		t.Fatalf("append after close should be dropped quietly, got %v", err)
	}
	if got, _ := s.Replay(0); len(got) != 1 {
		t.Fatalf("append after close was kept: %+v", got)
	}
}
