package fuse

import (
	"context"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/event"
)

// TestCompletionFiresExactlyOnParkedOverWire feeds a scripted stream ending in the
// loop's completion event (event.KindLoopParked) through the REMOTE SDK over a real
// Connect server, and asserts IsCompletion fires on EXACTLY that event and no earlier
// one — then that the caller can Send the next turn (persistent-loop parity). This ties
// the predicate to the real kind end-to-end, not just as a unit check (that lives in
// fuse_test.go).
func TestCompletionFiresExactlyOnParkedOverWire(t *testing.T) {
	live := make(chan event.Event, 8)
	f := &fakeRuntime{startID: "loop-c", live: live}
	f.hist = []event.Event{
		{Seq: 1, Kind: event.KindTurnStart},
		{Seq: 2, Kind: event.KindModelCallStart},
		{Seq: 3, Kind: event.KindModelCallEnd},
		{Seq: 4, Kind: event.KindLoopParked}, // the completion signal
	}
	base := remoteFakeServer(t, f, devVerifier())
	c := NewRemote(base, Credentials{Token: "good-tok", Tenant: string(event.DefaultTenant)})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, unsub, err := c.Observe(ctx, "loop-c", 0)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	defer unsub()

	var completionSeq Seq
	completions := 0
	deadline := time.After(5 * time.Second)
loop:
	for {
		select {
		case e, ok := <-ch:
			if !ok {
				break loop
			}
			if IsCompletion(e) {
				completions++
				completionSeq = e.Seq
			}
			if e.Kind == event.KindLoopParked {
				break loop // the exchange is complete; stop observing.
			}
		case <-deadline:
			t.Fatal("timeout waiting for completion event")
		}
	}

	if completions != 1 {
		t.Fatalf("IsCompletion fired %d times, want exactly 1", completions)
	}
	if completionSeq != 4 {
		t.Fatalf("completion fired on seq %d, want 4 (the parked event)", completionSeq)
	}

	// Persistent-loop parity: after completion the caller can Send the next turn.
	if err := c.Send(ctx, "loop-c", "next turn"); err != nil {
		t.Fatalf("Send after completion: %v", err)
	}
	if f.sawSendIn != "next turn" {
		t.Fatalf("post-completion Send not delivered: in=%q", f.sawSendIn)
	}
}
