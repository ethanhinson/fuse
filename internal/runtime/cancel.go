package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/event"
)

// cancel.go stops a loop and seals it (the Canceller capability).
//
// A cancel is two facts: an in-memory seal on the cached loop (loop.cancelled, so
// Send, Steer and Resume refuse it with ErrLoopCancelled) and a terminal
// loop.cancelled event, the highest seq the loop's stream ever holds. Who appends
// that event is decided under inProcRuntime.mu by two loop fields:
//
//   - a Cancel that lands while the run is still going (finishing == false) fires
//     the loop's abort; the run unwinds through context cancellation and the
//     completion goroutine appends loop.cancelled{was_live:true} before it marks the
//     loop not live and tears it down;
//   - a Cancel that lands once the completion goroutine has passed its decision
//     point (finishing == true: the run already returned) appends
//     loop.cancelled{was_live:false} itself.
//
// Exactly one of them appends. A second Cancel finds the seal already set and
// reports the first one (AlreadyCancelled) without appending.

var _ Canceller = (*inProcRuntime)(nil)

// defaultCancelWait bounds Cancel's wait for an unwind when Deps.CancelWait is zero.
const defaultCancelWait = 10 * time.Second

func (r *inProcRuntime) cancelWait() time.Duration {
	if r.deps.CancelWait > 0 {
		return r.deps.CancelWait
	}
	return defaultCancelWait
}

// cancelState is the record of the first cancel of one loop. reason, requestedBy,
// requestSeq and wasLive are fixed before the state is published on the loop (under
// inProcRuntime.mu) and never change after.
type cancelState struct {
	reason, requestedBy string
	requestSeq          event.Seq // the loop.cancel.request this answers; 0 for a local cancel
	wasLive             bool
	terminalSeq         event.Seq     // guarded by inProcRuntime.mu
	sealed              chan struct{} // closed once loop.cancelled is appended (or the append failed)
}

// isCancelled reports whether a Cancel has sealed lp.
func (r *inProcRuntime) isCancelled(lp *loop) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return lp.cancelled != nil
}

// Cancel stops a loop and seals it. See Canceller.
func (r *inProcRuntime) Cancel(ctx context.Context, tenant event.TenantID, loopID, reason, requestedBy string) (CancelResult, error) {
	if lp := r.loopCache(tenant, loopID); lp != nil {
		return r.cancelLocal(ctx, lp, reason, requestedBy, 0)
	}
	return CancelResult{}, fmt.Errorf("%w: %q", ErrLoopNotFound, loopID)
}

// cancelLocal cancels a loop held in this process's cache. It returns Cancelled
// only once the loop's completion has run (the registry shows it not live and its
// resources are released), and Cancelling when that does not happen within ctx and
// the CancelWait cap.
func (r *inProcRuntime) cancelLocal(ctx context.Context, lp *loop, reason, requestedBy string, requestSeq event.Seq) (CancelResult, error) {
	wctx, stop := context.WithTimeout(ctx, r.cancelWait())
	defer stop()

	r.mu.Lock()
	if cs := lp.cancelled; cs != nil {
		r.mu.Unlock()
		// An earlier cancel owns the terminal append: report it once it is recorded.
		if !waitClosed(wctx, cs.sealed) {
			return CancelResult{Outcome: CancelOutcomeCancelling, WasLive: cs.wasLive, Reason: cs.reason}, nil
		}
		// Best effort: let the completion finish too, so the loop reads not live.
		waitClosed(wctx, lp.handle.done)
		return CancelResult{Outcome: CancelOutcomeAlreadyCancelled, WasLive: cs.wasLive, TerminalSeq: r.terminalSeq(cs), Reason: cs.reason}, nil
	}
	cs := &cancelState{
		reason:      reason,
		requestedBy: requestedBy,
		requestSeq:  requestSeq,
		wasLive:     !lp.finishing,
		sealed:      make(chan struct{}),
	}
	lp.cancelled = cs
	r.mu.Unlock()

	if cs.wasLive {
		// The completion goroutine has not decided yet: it will see cs and append the
		// terminal event after the run unwinds.
		lp.abort(ErrLoopCancelled)
		if !waitClosed(wctx, lp.handle.done) {
			return CancelResult{Outcome: CancelOutcomeCancelling, WasLive: true, Reason: reason}, nil
		}
		return CancelResult{Outcome: CancelOutcomeCancelled, WasLive: true, TerminalSeq: r.terminalSeq(cs), Reason: reason}, nil
	}

	// The run had already returned: nothing to interrupt, so this call writes the
	// terminal event that seals the stream.
	seq := r.appendTerminal(lp, cs)
	r.mu.Lock()
	cs.terminalSeq = seq
	r.mu.Unlock()
	close(cs.sealed)
	if !waitClosed(wctx, lp.handle.done) {
		return CancelResult{Outcome: CancelOutcomeCancelling, Reason: reason}, nil
	}
	return CancelResult{Outcome: CancelOutcomeCancelled, TerminalSeq: seq, Reason: reason}, nil
}

func (r *inProcRuntime) terminalSeq(cs *cancelState) event.Seq {
	r.mu.Lock()
	defer r.mu.Unlock()
	return cs.terminalSeq
}

// waitClosed waits for ch to close or ctx to end, and reports whether ch closed.
func waitClosed(ctx context.Context, ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	case <-ctx.Done():
		select {
		case <-ch:
			return true
		default:
			return false
		}
	}
}

// awaitChildren is where a cancelled loop waits for its spawned children to
// record spawn.done before the terminal event. It does not wait yet.
func (r *inProcRuntime) awaitChildren(*agent.AgentTree) {}

// appendTerminal appends lp's loop.cancelled event and returns its seq (0 when the
// store cannot report it or the append failed; the in-memory seal holds either
// way). It writes straight to the store under context.Background(), not through
// the loop's sink, whose context is the session context the completion path ends.
func (r *inProcRuntime) appendTerminal(lp *loop, cs *cancelState) event.Seq {
	payload, err := json.Marshal(event.LoopCancelledPayload{
		Reason:      cs.reason,
		RequestedBy: cs.requestedBy,
		WasLive:     cs.wasLive,
		RequestSeq:  uint64(cs.requestSeq),
	})
	if err != nil {
		return 0
	}
	ev := event.Event{NodeID: lp.id, Kind: event.KindLoopCancelled, Payload: payload}
	if r.deps.DurableStore != nil {
		seq, _ := r.appendDurableTerminal(context.Background(), lp.key, ev)
		return seq
	}
	if err := lp.store.Append(ev); err != nil {
		return 0
	}
	evs, err := lp.store.Replay(0)
	if err != nil {
		return 0
	}
	return lastSeqOfKind(evs, event.KindLoopCancelled)
}

// appendDurableTerminal appends a terminal event to the durable stream key and
// returns the seq the store gave it. A store that reports its committed envelope
// gives the seq directly; otherwise the stream is replayed to find it (0 if that
// read fails after a successful append).
func (r *inProcRuntime) appendDurableTerminal(ctx context.Context, key event.StreamKey, ev event.Event) (event.Seq, error) {
	ds := r.deps.DurableStore
	if cds, ok := ds.(event.CommittedDurableStore); ok {
		got, err := cds.AppendCommitted(ctx, key, ev)
		if err != nil {
			return 0, err
		}
		return got.Seq, nil
	}
	if err := ds.Append(ctx, key, ev); err != nil {
		return 0, err
	}
	evs, err := ds.Replay(ctx, key, 0)
	if err != nil {
		return 0, nil
	}
	return lastSeqOfKind(evs, ev.Kind), nil
}

// lastSeqOfKind returns the highest seq of an event of kind k in evs, or 0.
func lastSeqOfKind(evs []event.Event, k event.Kind) event.Seq {
	var seq event.Seq
	for _, e := range evs {
		if e.Kind == k && e.Seq > seq {
			seq = e.Seq
		}
	}
	return seq
}
