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
//     loop.cancelled{was_live:false} itself. Over a durable store the cached loop
//     is only a leftover, so that Cancel defers to the stream and the registry
//     instead (cancelFinished): another replica may already have sealed the stream,
//     or resumed the loop and be running it.
//
// Exactly one of them appends. A second Cancel finds the seal already set and
// reports the first one (AlreadyCancelled) without appending.
//
// A loop this process does not hold is resolved through the durable registry:
//
//   - not live (finished, reaped, drained, or its owner's lease expired): the stream
//     is sealed here by appending loop.cancelled{was_live:false}, unless it already
//     holds one;
//   - live on another replica: a loop.cancel.request is appended to its stream and
//     the owner's relay pump honours it (relay.go). This side waits, bounded, for the
//     owner's loop.cancelled and for the registry to show the loop not live.
//
// Resume refuses a stream that holds loop.cancelled, and answers an unanswered
// loop.cancel.request with the terminal event instead of reviving the loop. When a
// stream holds two loop.cancelled events (two replicas sealing it at once), every
// reader takes the first in seq order.

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
	if r.deps.DurableStore == nil {
		return CancelResult{}, fmt.Errorf("%w: %q", ErrLoopNotFound, loopID)
	}
	// Tenant-scoped, and an expired lease is marked not live (never re-owned):
	// nothing here runs the loop.
	rec, key, err := r.resolveDurableForDelivery(ctx, tenant, loopID)
	if err != nil {
		return CancelResult{}, err
	}
	if !rec.Live {
		return r.sealNotLive(ctx, key, &event.LoopCancelRequestPayload{Reason: reason, RequestedBy: requestedBy})
	}
	return r.relayCancel(ctx, key, reason, requestedBy)
}

// sealNotLive seals the stream of a loop no process runs by appending
// loop.cancelled{was_live:false}, answering the stream's unanswered
// loop.cancel.request if it has one. by is the caller sealing it; nil answers the
// pending request under that request's own reason and requester (a resume), and
// appends nothing when no request is pending. A stream that already holds
// loop.cancelled is reported as AlreadyCancelled and left untouched.
//
// cancelMu makes the read and the append one step within this process, so
// concurrent seals here append once. Two replicas sealing at once can still append
// two; the first wins.
func (r *inProcRuntime) sealNotLive(ctx context.Context, key event.StreamKey, by *event.LoopCancelRequestPayload) (CancelResult, error) {
	ds := r.deps.DurableStore
	r.cancelMu.Lock()
	defer r.cancelMu.Unlock()
	evs, err := ds.Replay(ctx, key, 0)
	if err != nil {
		return CancelResult{}, err
	}
	if e, p, ok := firstCancelled(evs); ok {
		r.dropCancelPending(key)
		return CancelResult{Outcome: CancelOutcomeAlreadyCancelled, WasLive: p.WasLive, TerminalSeq: e.Seq, Reason: p.Reason}, nil
	}
	answer := event.LoopCancelledPayload{}
	if req, rp, ok := pendingCancel(evs); ok {
		answer.RequestSeq = uint64(req.Seq)
		answer.Reason, answer.RequestedBy = rp.Reason, rp.RequestedBy
	} else if by == nil {
		return CancelResult{}, nil
	}
	if by != nil {
		// This caller sealed it: the terminal carries the caller's reason.
		answer.Reason, answer.RequestedBy = by.Reason, by.RequestedBy
	}
	payload, err := json.Marshal(answer)
	if err != nil {
		return CancelResult{}, err
	}
	// Once decided, the seal is not abandoned because the caller went away.
	seq, err := r.appendDurableWithSeq(context.WithoutCancel(ctx), key,
		event.Event{NodeID: string(key.Loop), Kind: event.KindLoopCancelled, Payload: payload})
	if err != nil {
		return CancelResult{}, fmt.Errorf("runtime: record cancel of %q: %w", key.Loop, err)
	}
	r.dropCancelPending(key)
	return CancelResult{Outcome: CancelOutcomeCancelled, TerminalSeq: seq, Reason: answer.Reason}, nil
}

// refuseCancelledResume reports whether a resume of the stream evs must be refused
// because the loop was cancelled: the stream holds loop.cancelled, or it holds a
// loop.cancel.request nobody answered (the owner went away first), which is answered
// here with the terminal event. The returned error wraps ErrLoopCancelled.
func (r *inProcRuntime) refuseCancelledResume(ctx context.Context, key event.StreamKey, evs []event.Event) (bool, error) {
	refused := fmt.Errorf("%w: %q", ErrLoopCancelled, key.Loop)
	if _, _, ok := firstCancelled(evs); ok {
		return true, refused
	}
	if _, _, ok := pendingCancel(evs); !ok {
		return false, nil
	}
	if _, err := r.sealNotLive(ctx, key, nil); err != nil {
		// The loop was still asked to stop: refuse it even though the terminal event
		// could not be written.
		return true, fmt.Errorf("%w (%v)", refused, err)
	}
	return true, refused
}

// Relay cancel bounds.
const relayCancelRegistryPoll = 10 * time.Millisecond

// relayCancel asks the replica that runs key's loop to cancel it, through a
// loop.cancel.request in the loop's stream, then waits (bounded by ctx and
// CancelWait) for the owner's loop.cancelled and for the registry to show the loop
// not live: only both make Cancelled. Otherwise it reports Cancelling.
//
// The request this process appended is remembered in cancelPending until its answer
// is seen, so a repeat Cancel from here appends no second request: it reports the
// terminal event once one exists (AlreadyCancelled) and Cancelling until then.
func (r *inProcRuntime) relayCancel(ctx context.Context, key event.StreamKey, reason, requestedBy string) (CancelResult, error) {
	ds := r.deps.DurableStore
	r.cancelMu.Lock()
	r.mu.Lock()
	_, pending := r.cancelPending[key]
	r.mu.Unlock()
	evs, err := ds.Replay(ctx, key, 0)
	if err != nil {
		r.cancelMu.Unlock()
		return CancelResult{}, err
	}
	if e, p, ok := firstCancelled(evs); ok {
		r.cancelMu.Unlock()
		r.dropCancelPending(key)
		return CancelResult{Outcome: CancelOutcomeAlreadyCancelled, WasLive: p.WasLive, TerminalSeq: e.Seq, Reason: p.Reason}, nil
	}
	if pending {
		r.cancelMu.Unlock()
		return CancelResult{Outcome: CancelOutcomeCancelling, Reason: reason}, nil
	}
	payload, err := json.Marshal(event.LoopCancelRequestPayload{Reason: reason, RequestedBy: requestedBy})
	if err != nil {
		r.cancelMu.Unlock()
		return CancelResult{}, err
	}
	reqSeq, err := r.appendDurableWithSeq(ctx, key,
		event.Event{NodeID: string(key.Loop), Kind: event.KindLoopCancelRequest, Payload: payload})
	if err != nil {
		r.cancelMu.Unlock()
		return CancelResult{}, fmt.Errorf("runtime: relay cancel of %q: %w", key.Loop, err)
	}
	r.mu.Lock()
	if r.cancelPending == nil {
		r.cancelPending = map[event.StreamKey]event.Seq{}
	}
	r.cancelPending[key] = reqSeq
	r.mu.Unlock()
	r.cancelMu.Unlock()

	wctx, stop := context.WithTimeout(ctx, r.cancelWait())
	defer stop()
	term, tp, ok := r.awaitTerminal(wctx, key)
	if !ok {
		return CancelResult{Outcome: CancelOutcomeCancelling, Reason: reason}, nil
	}
	if !r.awaitNotLive(wctx, key) {
		return CancelResult{Outcome: CancelOutcomeCancelling, WasLive: tp.WasLive, Reason: tp.Reason}, nil
	}
	r.dropCancelPending(key)
	return CancelResult{Outcome: CancelOutcomeCancelled, WasLive: tp.WasLive, TerminalSeq: term.Seq, Reason: tp.Reason}, nil
}

// awaitTerminal waits until key's stream holds a loop.cancelled or ctx ends. It
// subscribes first and then replays, so an event appended in between is not missed;
// live events at or below the replay watermark were already read and are skipped.
// The watermark moves only with replays, so an event the subscription dropped is
// still found by the periodic re-read.
func (r *inProcRuntime) awaitTerminal(ctx context.Context, key event.StreamKey) (event.Event, event.LoopCancelledPayload, bool) {
	ds := r.deps.DurableStore
	wake, unsub, err := ds.Subscribe(ctx, key)
	if err != nil {
		wake = nil // the periodic read still works
	} else {
		defer unsub()
	}
	var watermark event.Seq
	read := func() (event.Event, event.LoopCancelledPayload, bool) {
		evs, err := ds.Replay(ctx, key, watermark)
		if err != nil {
			return event.Event{}, event.LoopCancelledPayload{}, false
		}
		for _, e := range evs {
			if e.Seq > watermark {
				watermark = e.Seq
			}
		}
		return firstCancelled(evs)
	}
	if e, p, ok := read(); ok {
		return e, p, true
	}
	tick := time.NewTicker(r.relayPoll())
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return event.Event{}, event.LoopCancelledPayload{}, false
		case ev, open := <-wake:
			if !open {
				wake = nil
				continue
			}
			if ev.Seq <= watermark || ev.Kind != event.KindLoopCancelled {
				continue
			}
			if e, p, ok := firstCancelled([]event.Event{ev}); ok {
				return e, p, true
			}
		case <-tick.C:
			if e, p, ok := read(); ok {
				return e, p, true
			}
		}
	}
}

// awaitNotLive polls the registry until key's loop reads not live or ctx ends.
func (r *inProcRuntime) awaitNotLive(ctx context.Context, key event.StreamKey) bool {
	tick := time.NewTicker(relayCancelRegistryPoll)
	defer tick.Stop()
	for {
		if rec, err := r.deps.Registry.Resolve(ctx, key); err == nil && !rec.Live {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-tick.C:
		}
	}
}

func (r *inProcRuntime) dropCancelPending(key event.StreamKey) {
	r.mu.Lock()
	delete(r.cancelPending, key)
	r.mu.Unlock()
}

// firstCancelled returns the first loop.cancelled in evs by seq: the authoritative
// one if a race left two. A payload that does not decode still counts (the loop is
// sealed either way) and yields a zero payload.
func firstCancelled(evs []event.Event) (event.Event, event.LoopCancelledPayload, bool) {
	var first event.Event
	found := false
	for _, e := range evs {
		if e.Kind == event.KindLoopCancelled && (!found || e.Seq < first.Seq) {
			first, found = e, true
		}
	}
	if !found {
		return event.Event{}, event.LoopCancelledPayload{}, false
	}
	var p event.LoopCancelledPayload
	_ = json.Unmarshal(first.Payload, &p)
	return first, p, true
}

// pendingCancel returns the first loop.cancel.request in evs that no loop.cancelled
// at a higher seq answers. A request whose payload does not decode is skipped, as
// the relay pump skips it.
func pendingCancel(evs []event.Event) (event.Event, event.LoopCancelRequestPayload, bool) {
	var answered event.Seq
	for _, e := range evs {
		if e.Kind == event.KindLoopCancelled && e.Seq > answered {
			answered = e.Seq
		}
	}
	var (
		first event.Event
		fp    event.LoopCancelRequestPayload
		found bool
	)
	for _, e := range evs {
		if e.Kind != event.KindLoopCancelRequest || e.Seq <= answered || (found && e.Seq > first.Seq) {
			continue
		}
		var p event.LoopCancelRequestPayload
		if json.Unmarshal(e.Payload, &p) != nil {
			continue
		}
		first, fp, found = e, p, true
	}
	return first, fp, found
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
	if lp.finishing && r.deps.DurableStore != nil {
		// The run already returned, so this process no longer decides alone: the
		// stream and the registry it shares with other replicas do.
		r.mu.Unlock()
		return r.cancelFinished(wctx, lp, reason, requestedBy, requestSeq)
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

	// The run had already returned and no other process shares this loop's stream:
	// nothing to interrupt, so this call writes the terminal event that seals it.
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

// cancelFinished cancels a cached loop whose run already returned, over a durable
// store. The cached loop is only a leftover of this replica's run: another replica
// may have sealed the stream since, or resumed the loop and be running it now. So the
// stream and the registry decide the outcome, never the cache:
//
//   - not live: the stream is sealed through sealNotLive, which reports a terminal
//     event already there (AlreadyCancelled) instead of appending a second one;
//   - live: another replica resumed the loop (this one never relaunches a loop it
//     caches), so the cancel is relayed to it. A request the relay pump handed here
//     is already in the stream, where that replica's own pump answers it.
//
// Once the stream holds the terminal event, lp is sealed in memory with it, so a
// later cancel here reports it without reading the stream again.
func (r *inProcRuntime) cancelFinished(ctx context.Context, lp *loop, reason, requestedBy string, requestSeq event.Seq) (CancelResult, error) {
	// Let the completion record the loop not live first, so a live record below
	// can only mean another replica runs it.
	if !waitClosed(ctx, lp.handle.done) {
		return CancelResult{Outcome: CancelOutcomeCancelling, Reason: reason}, nil
	}
	live := false
	if r.deps.Registry != nil {
		rec, _, err := r.resolveDurableForDelivery(ctx, lp.key.Tenant, string(lp.key.Loop))
		if err != nil {
			return CancelResult{}, err
		}
		live = rec.Live
	}
	var (
		res CancelResult
		err error
	)
	switch {
	case live && requestSeq != 0:
		return CancelResult{Outcome: CancelOutcomeCancelling, Reason: reason}, nil
	case live:
		res, err = r.relayCancel(ctx, lp.key, reason, requestedBy)
	default:
		res, err = r.sealNotLive(ctx, lp.key, &event.LoopCancelRequestPayload{Reason: reason, RequestedBy: requestedBy})
	}
	if err != nil || res.Outcome == CancelOutcomeCancelling {
		return res, err
	}
	r.mu.Lock()
	if lp.cancelled == nil {
		cs := &cancelState{
			reason:      res.Reason,
			requestSeq:  requestSeq,
			wasLive:     res.WasLive,
			terminalSeq: res.TerminalSeq,
			sealed:      make(chan struct{}),
		}
		close(cs.sealed)
		lp.cancelled = cs
	}
	r.mu.Unlock()
	return res, nil
}

// cancelRelayed honours a cancel the relay pump found in lp's stream. A request is
// a local cancel that records which request it answers. A loop.cancelled another
// replica already wrote (it sealed the loop while it read not live, racing this
// replica starting it) seals lp in memory without a second terminal event when the
// run has already returned; a run still going is aborted, and its completion appends
// its own loop.cancelled so the terminal event stays the last one (readers take the
// first).
func (r *inProcRuntime) cancelRelayed(lp *loop, c relayedCancel) {
	if c.sealedSeq == 0 {
		_, _ = r.cancelLocal(context.Background(), lp, c.reason, c.requestedBy, c.requestSeq)
		return
	}
	r.mu.Lock()
	if lp.cancelled != nil {
		r.mu.Unlock()
		return
	}
	cs := &cancelState{
		reason:      c.reason,
		requestedBy: c.requestedBy,
		requestSeq:  c.requestSeq,
		wasLive:     !lp.finishing,
		sealed:      make(chan struct{}),
	}
	lp.cancelled = cs
	if !cs.wasLive {
		cs.terminalSeq = c.sealedSeq
		close(cs.sealed)
		r.mu.Unlock()
		return
	}
	r.mu.Unlock()
	lp.abort(ErrLoopCancelled)
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

// Bounds for awaitChildren's poll.
const (
	awaitChildrenPoll = 5 * time.Millisecond
	awaitChildrenCap  = 5 * time.Second
)

// awaitChildren holds a cancelled loop's terminal event until its spawned
// children have unwound, so every spawn.done precedes loop.cancelled and every
// scheduler slot is free once Cancel reports Cancelled. A child's node status
// flips before its spawn.done is emitted, so the wait keys on the tree's in-flight
// spawn count as well as its running/pending nodes. It gives up after
// awaitChildrenCap: a child that ignores cancellation must not hold the seal.
func (r *inProcRuntime) awaitChildren(tree *agent.AgentTree) {
	if tree == nil {
		return
	}
	idle := func() bool {
		running, pending := tree.ActiveCounts()
		return running == 0 && pending == 0 && tree.SpawnsInFlight() == 0
	}
	if idle() {
		return
	}
	deadline := time.NewTimer(awaitChildrenCap)
	defer deadline.Stop()
	tick := time.NewTicker(awaitChildrenPoll)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			if idle() {
				return
			}
		case <-deadline.C:
			return
		}
	}
}

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
		seq, _ := r.appendDurableWithSeq(context.Background(), lp.key, ev)
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

// appendDurableWithSeq appends ev to the durable stream key and returns the seq
// the store gave it. A store that reports its committed envelope gives the seq
// directly; otherwise the stream is replayed to find it (the highest seq of ev's
// kind; 0 if that read fails after a successful append).
func (r *inProcRuntime) appendDurableWithSeq(ctx context.Context, key event.StreamKey, ev event.Event) (event.Seq, error) {
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
