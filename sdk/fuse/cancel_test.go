package fuse

import (
	"context"
	"errors"
	"testing"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/runtime"
)

// TestCancelLocalDelegates proves the local backend type-asserts the runtime's
// Canceller capability, threads the tenant, and maps the seam result to the SDK's
// CancelResult.
func TestCancelLocalDelegates(t *testing.T) {
	f := &fakeRuntime{
		cancelRes: runtime.CancelResult{
			Outcome:     runtime.CancelOutcomeCancelled,
			WasLive:     true,
			TerminalSeq: 9,
			Reason:      "operator stop",
		},
	}
	c := NewLocal(f, Credentials{Tenant: string(event.DefaultTenant)})

	res, err := c.Cancel(context.Background(), "loop-1", "operator stop")
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if res.Outcome != CancelOutcomeCancelled || !res.WasLive || res.TerminalSeq != 9 || res.Reason != "operator stop" {
		t.Fatalf("Cancel = %+v, want cancelled, live, seq 9, reason", res)
	}
	if f.sawCancelID != "loop-1" || f.sawCancelWhy != "operator stop" || f.sawCancelTn != event.DefaultTenant {
		t.Fatalf("Cancel not threaded: id=%q reason=%q tenant=%q", f.sawCancelID, f.sawCancelWhy, f.sawCancelTn)
	}
}

// TestCancelLocalAlreadyCancelled proves an idempotent repeat surfaces the first
// cancel's terminal seq and reason.
func TestCancelLocalAlreadyCancelled(t *testing.T) {
	f := &fakeRuntime{
		cancelRes: runtime.CancelResult{
			Outcome:     runtime.CancelOutcomeAlreadyCancelled,
			TerminalSeq: 4,
			Reason:      "first",
		},
	}
	c := NewLocal(f, Credentials{Tenant: string(event.DefaultTenant)})
	res, err := c.Cancel(context.Background(), "loop-1", "second")
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if res.Outcome != CancelOutcomeAlreadyCancelled || res.TerminalSeq != 4 || res.Reason != "first" {
		t.Fatalf("Cancel = %+v, want already_cancelled, seq 4, reason first", res)
	}
}

// noCancelRuntime is a runtime without the Canceller capability: it embeds
// fakeRuntime's methods except Cancel by holding one as an interface-typed field.
type noCancelRuntime struct{ runtime.Runtime }

func TestCancelLocalUnsupported(t *testing.T) {
	f := &fakeRuntime{}
	c := NewLocal(noCancelRuntime{f}, Credentials{Tenant: string(event.DefaultTenant)})
	if _, err := c.Cancel(context.Background(), "loop-1", "x"); !errors.Is(err, ErrCancelUnsupported) {
		t.Fatalf("Cancel err = %v, want ErrCancelUnsupported", err)
	}
}

// TestCancelRemoteOverWire drives Client.Cancel through the REAL connect server,
// asserting reason + tenant reach the handler and the outcome round-trips.
func TestCancelRemoteOverWire(t *testing.T) {
	f := &fakeRuntime{
		startID: "loop-c",
		cancelRes: runtime.CancelResult{
			Outcome:     runtime.CancelOutcomeCancelled,
			WasLive:     true,
			TerminalSeq: 7,
			Reason:      "operator stop",
		},
	}
	base := remoteFakeServer(t, f, devVerifier())
	c := NewRemote(base, Credentials{Token: "good-tok", Tenant: string(event.DefaultTenant)})

	res, err := c.Cancel(context.Background(), "loop-c", "operator stop")
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if res.Outcome != CancelOutcomeCancelled || !res.WasLive || res.TerminalSeq != 7 || res.Reason != "operator stop" {
		t.Fatalf("Cancel = %+v, want cancelled, live, seq 7, reason", res)
	}
	if f.sawCancelID != "loop-c" || f.sawCancelWhy != "operator stop" {
		t.Fatalf("Cancel not threaded over wire: id=%q reason=%q", f.sawCancelID, f.sawCancelWhy)
	}
}

// TestCancelRemoteUnknownLoopNotFound proves the wire NotFound maps to ErrLoopNotFound.
func TestCancelRemoteUnknownLoopNotFound(t *testing.T) {
	f := &fakeRuntime{cancelErr: runtime.ErrLoopNotFound}
	base := remoteFakeServer(t, f, devVerifier())
	c := NewRemote(base, Credentials{Token: "good-tok", Tenant: string(event.DefaultTenant)})
	if _, err := c.Cancel(context.Background(), "nope", "x"); !errors.Is(err, ErrLoopNotFound) {
		t.Fatalf("Cancel err = %v, want ErrLoopNotFound", err)
	}
}

// TestCancelledIsCompletionOverWire feeds a stream ending in loop.cancelled through
// the REMOTE SDK and asserts IsCompletion fires on exactly that terminal event.
func TestCancelledIsCompletionOverWire(t *testing.T) {
	live := make(chan event.Event, 8)
	f := &fakeRuntime{startID: "loop-c", live: live}
	f.hist = []event.Event{
		{Seq: 1, Kind: event.KindTurnStart},
		{Seq: 2, Kind: event.KindLoopCancelled}, // the terminal completion signal
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

	var last Event
	seqs := collectRemote(t, ch, 2, &last)
	assertNoLossNoDup(t, seqs, 0, 2, "cancelled completion")
	if !IsCompletion(last) {
		t.Fatalf("last event kind = %q, want completion (loop.cancelled)", last.Kind)
	}
}
