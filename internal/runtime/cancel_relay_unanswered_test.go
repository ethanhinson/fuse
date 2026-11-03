package runtime

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/event/fsstore"
)

// registerGhostOwner records id as live under an owner that is not one of the test's
// replicas, so a cancel from a replica is relayed through the stream and only the
// test decides what the "owner" does with it.
func registerGhostOwner(t *testing.T, store *fsstore.FSDurableStore, id string, lease time.Time) event.StreamKey {
	t.Helper()
	key := event.StreamKey{Tenant: event.DefaultTenant, Loop: event.LoopID(id)}
	if err := store.Register(context.Background(), event.LoopRecord{Key: key, OwnerNodeID: "ghost", Live: true, LeaseExpiry: lease}); err != nil {
		t.Fatal(err)
	}
	return key
}

// cancelAsync runs a Cancel on c in its own goroutine and hands back its result.
func cancelAsync(c Canceller, id, reason, by string) <-chan CancelResult {
	out := make(chan CancelResult, 1)
	go func() {
		res, err := c.Cancel(context.Background(), event.DefaultTenant, id, reason, by)
		if err != nil {
			res = CancelResult{Outcome: CancelOutcomeUnspecified, Reason: "error: " + err.Error()}
		}
		out <- res
	}()
	return out
}

// assertRequesterSealed checks a relayed cancel the owner never answered was sealed by
// the requester: one loop.cancelled{was_live:false} answering the one request, last in
// the stream, reported as Cancelled with its seq.
func assertRequesterSealed(t *testing.T, store *fsstore.FSDurableStore, id string, res CancelResult) {
	t.Helper()
	if res.Outcome != CancelOutcomeCancelled || res.WasLive || res.Reason != "stop" {
		t.Fatalf("cancel = %+v, want Cancelled, not was_live, reason stop", res)
	}
	evs := streamOf(t, store, event.DefaultTenant, id)
	reqs := cancelRequests(t, evs)
	if len(reqs) != 1 {
		t.Fatalf("want exactly one loop.cancel.request, got %d", len(reqs))
	}
	if n := countKind(evs, event.KindLoopCancelled); n != 1 || lastKind(evs) != event.KindLoopCancelled {
		t.Fatalf("want one terminal loop.cancelled, got %d (last %s)", n, lastKind(evs))
	}
	last := evs[len(evs)-1]
	p := cancelledPayload(t, last)
	if p.WasLive || p.RequestSeq != uint64(reqs[0].Seq) || p.Reason != "stop" || p.RequestedBy != "alice" {
		t.Fatalf("terminal payload = %+v, want stop/alice/not was_live/request_seq %d", p, reqs[0].Seq)
	}
	if res.TerminalSeq != last.Seq {
		t.Fatalf("TerminalSeq = %d, want %d", res.TerminalSeq, last.Seq)
	}
	if isLive(t, store, event.DefaultTenant, id) {
		t.Fatal("Cancelled reported while the loop reads live")
	}
}

// The owner's run finishes (a one-shot completing, an idle reap) after the requester
// appended its loop.cancel.request but before the owner's relay pump read it: the
// owner records the loop not live without a loop.cancelled. Nobody else will answer
// the request, so the requester seals the stream itself rather than reporting
// Cancelling for a loop that is never sealed.
func TestRelayedCancelOwnerFinishesWithoutAnswering(t *testing.T) {
	f := &steerFake{}
	_, rtB, store := twoReplicasWith(t, f, nil, withCancelWait(3*time.Second))
	const id = "finishing-loop"
	key := registerGhostOwner(t, store, id, time.Now().Add(time.Hour))

	done := cancelAsync(rtB.(Canceller), id, "stop", "alice")
	eventually(t, func() bool { return len(cancelRequests(t, streamOf(t, store, event.DefaultTenant, id))) == 1 })
	// The owner's run completes normally: not live, and no terminal event.
	if err := store.SetLive(context.Background(), key, false, "ghost"); err != nil {
		t.Fatal(err)
	}

	var res CancelResult
	select {
	case res = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Cancel did not return")
	}
	assertRequesterSealed(t, store, id, res)

	n := len(streamOf(t, store, event.DefaultTenant, id))
	again, err := rtB.(Canceller).Cancel(context.Background(), event.DefaultTenant, id, "again", "bob")
	if err != nil {
		t.Fatal(err)
	}
	if again.Outcome != CancelOutcomeAlreadyCancelled || again.TerminalSeq != res.TerminalSeq || again.Reason != "stop" {
		t.Fatalf("repeat cancel = %+v, want AlreadyCancelled stop @%d", again, res.TerminalSeq)
	}
	if got := len(streamOf(t, store, event.DefaultTenant, id)); got != n {
		t.Fatalf("the repeat cancel appended events (%d -> %d)", n, got)
	}
}

// The owner dies while the requester waits: its lease expires and nothing answers the
// request. The requester sees the expired lease, marks the loop not live and seals the
// stream.
func TestRelayedCancelOwnerDiesWhileWaiting(t *testing.T) {
	f := &steerFake{}
	var skew atomic.Int64
	nowB := func() time.Time { return time.Now().Add(time.Duration(skew.Load())) }
	_, rtB, store := twoReplicasWith(t, f, nowB, withCancelWait(3*time.Second))
	const id = "orphaned-loop"
	registerGhostOwner(t, store, id, time.Now().Add(time.Minute))

	done := cancelAsync(rtB.(Canceller), id, "stop", "alice")
	eventually(t, func() bool { return len(cancelRequests(t, streamOf(t, store, event.DefaultTenant, id))) == 1 })
	// The owner stops renewing its lease, and the lease runs out.
	skew.Store(int64(time.Hour))

	var res CancelResult
	select {
	case res = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Cancel did not return")
	}
	assertRequesterSealed(t, store, id, res)
}

// A relayed cancel whose wait runs out forgets its pending request: a later cancel
// still appends no second request (the stream shows one is pending), and the
// process-local set does not keep an entry for every loop whose cancel timed out.
func TestRelayedCancelTimeoutDropsPendingEntry(t *testing.T) {
	f := &steerFake{}
	_, rtB, store := twoReplicasWith(t, f, nil, withCancelWait(30*time.Millisecond))
	const id = "slow-owner-loop"
	registerGhostOwner(t, store, id, time.Now().Add(time.Hour))
	r := rtB.(*inProcRuntime)

	for i := 0; i < 2; i++ {
		res, err := r.Cancel(context.Background(), event.DefaultTenant, id, "stop", "alice")
		if err != nil {
			t.Fatalf("cancel %d: %v", i, err)
		}
		if res.Outcome != CancelOutcomeCancelling {
			t.Fatalf("cancel %d = %+v, want Cancelling", i, res)
		}
		r.mu.Lock()
		left := len(r.cancelPending)
		r.mu.Unlock()
		if left != 0 {
			t.Fatalf("cancel %d left %d pending entries after its wait ended", i, left)
		}
	}
	if n := len(cancelRequests(t, streamOf(t, store, event.DefaultTenant, id))); n != 1 {
		t.Fatalf("want exactly one loop.cancel.request, got %d", n)
	}
}
