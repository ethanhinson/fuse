package runtime

import (
	"context"
	"strconv"
	"strings"
	"sync"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/observe"
)

// turnTracer owns the turn-scoped span topology of ONE INTERACTIVE loop (change
// 0071, spec D1–D3). A one-shot loop never constructs one: its single
// fuse.loop.run span still covers the whole run and ends at completion, so the
// non-interactive trace shape is byte-identical to before this change.
//
// The problem it solves: an interactive session parks at each turn boundary and
// can live for hours, but OTEL's BatchSpanProcessor only exports a span at End()
// — so a session-scoped root never reaches the backend while the session is
// alive, and never at all if the process dies uncleanly. Instead:
//
//   - fuse.loop.run ends (success) at the FIRST park, covering startup plus the
//     first turn (index 1), and exports promptly;
//   - every later wake→park cycle gets its own fuse.loop.turn ROOT span, linked
//     back to the session's durable trace carrier via the existing delayed-carrier
//     idiom (observe.StartFromCarrier(..., delayed=true)) rather than any new
//     Observer surface (ADR-0040 provider neutrality).
//
// Turn indices are 1-based and index 1 is the turn INSIDE fuse.loop.run, so the
// first fuse.loop.turn span carries fuse.turn.index = 2.
//
// Concurrency: park/wake run synchronously on the run goroutine (see
// agent.HumanInjector.SetTurnBoundary), teardown runs on the same goroutine after
// Run returns — but activeTurn is read by every span-starting goroutine in the
// loop (spawned children included), so all state is mutex-guarded.
type turnTracer struct {
	// observer is the RAW Deps.Observer, never the turn-scoped wrapper below:
	// a turn root must not re-parent onto itself.
	observer observe.Observer
	// ctx is the session context turn roots are started from. Its own span
	// context is irrelevant (a turn is started as a new root); it carries the
	// session's values.
	ctx context.Context
	// link is the durable trace carrier of the SESSION root — fuse.loop.run for a
	// fresh interactive start, the carrier restored from the durable stream for a
	// resume. Every turn root carries a causal link to it, which is what keeps the
	// per-turn traces attributable to one session across park and resume.
	link *event.TraceCarrier
	// fields are the per-loop attributes every turn root carries (loop_id, tenant),
	// held immutably; fuse.turn.index is appended per turn.
	fields []observe.Field

	mu sync.Mutex
	// index is the index of the most recently STARTED turn (see the 1-based
	// convention above).
	index int
	// open is the currently-open turn span, or nil when the loop is parked (or
	// still inside the first turn, which fuse.loop.run covers).
	open observe.Handle
	// turn is open's own durable carrier — the parent every child span of this
	// turn attaches to (see turnScopedObserver).
	turn *event.TraceCarrier
	// closed is set at teardown so a late wake can never start a span nothing
	// would ever end.
	closed bool
}

// newTurnTracer builds the per-loop tracer. startIndex is the index of the last
// COMPLETED turn (1 for a fresh interactive start, whose first turn is covered by
// fuse.loop.run), so the first turn this tracer starts is startIndex+1.
func newTurnTracer(observer observe.Observer, ctx context.Context, link *event.TraceCarrier, fields []observe.Field, startIndex int) *turnTracer {
	if startIndex < 1 {
		startIndex = 1
	}
	return &turnTracer{observer: observer, ctx: ctx, link: link, fields: fields, index: startIndex}
}

// wake starts the next turn's root span. It is the injector's onWake callback:
// the parked loop has a human message to answer, which is the start of a turn.
// A resumed loop calls it once at launch, because the resume itself is a wake.
func (t *turnTracer) wake() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || t.open != nil {
		return
	}
	t.index++
	fields := make([]observe.Field, 0, len(t.fields)+1)
	fields = append(fields, t.fields...)
	fields = append(fields, observe.Field{Key: "fuse.turn.index", Value: strconv.Itoa(t.index)})
	// delayed=true is the repository's existing "new root, causally linked to the
	// durable producer" idiom: the turn is genuinely a new unit of work, not a
	// child of a span that has already ended. With a nil link (an untraced original
	// session, or a provider without propagation) the turn is an UNLINKED root — not
	// a child of t.ctx's span, which would silently stop it being a root at all and
	// defeat the whole change. That guarantee lives in the adapter: delayed work with
	// no recoverable carrier starts a new root (observe/otel.Observer.StartFromCarrier).
	ctx, h := observe.StartFromCarrier(t.observer, t.ctx, t.link, true, observe.Descriptor{
		Kind:   observe.OperationLoop,
		Name:   "turn",
		Fields: fields,
	})
	t.open = h
	t.turn = observe.TraceCarrier(t.observer, ctx)
}

// park closes the current turn. It is the injector's onPark callback, invoked
// just before the loop blocks awaiting the next human message.
//
// With no turn span open this is the FIRST park, which is the moment
// fuse.loop.run's work is done (spec D1): endRun ends it with success. endRun is
// endOnce-guarded and first-writer-wins, so a later cancellation or reap cannot
// rewrite the already-exported session root.
func (t *turnTracer) park(endRun func(observe.Outcome)) {
	if t == nil {
		return
	}
	t.mu.Lock()
	h := t.open
	t.open, t.turn = nil, nil
	t.mu.Unlock()
	if h != nil {
		// A park is the turn's clean terminal condition — the same condition under
		// which fuse.loop.run ends successfully at the first park.
		h.End(observe.OutcomeSuccess)
		return
	}
	endRun(observe.OutcomeSuccess)
}

// teardown ends any still-open turn span with the run's terminal outcome and
// permanently closes the tracer. It is the defensive half of D1/D3: a session
// that dies mid-turn (idle reap, disconnect-then-reap, run error) must not leave
// an unexported span behind. Safe to call more than once.
func (t *turnTracer) teardown(out observe.Outcome) {
	if t == nil {
		return
	}
	t.mu.Lock()
	h := t.open
	t.open, t.turn, t.closed = nil, nil, true
	t.mu.Unlock()
	if h != nil {
		h.End(out)
	}
}

// activeTurn returns a defensive copy of the open turn's carrier, or nil when no
// turn span is open.
func (t *turnTracer) activeTurn() *event.TraceCarrier {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.turn == nil {
		return nil
	}
	c := *t.turn
	return &c
}

// turnScopedObserver is the parenting seam (spec D2: "child spans of the turn
// parent to the turn span through the context"). The agent receives its run
// context ONCE, at Agent.Run, and that context's span is fuse.loop.run — which,
// for an interactive loop, has already ENDED by the time the second turn runs.
// Left alone, every later turn's model/tool/spawn span would attach to a dead
// parent in a trace that was already exported.
//
// The runtime cannot hand the agent a new context per turn without widening
// Agent's public surface, which this change explicitly does not do. It does
// already choose the agent's observer (a.SetObserver), so the re-parent happens
// there instead: when a turn root is open, a span whose incoming context belongs
// to a DIFFERENT trace is started from the turn's carrier (delayed=false ⇒ a real
// child), while a span already inside the turn's own trace is passed through
// untouched so natural nesting (tool under model attempt) is preserved.
//
// It is installed ONLY for interactive loops; a one-shot agent still receives
// Deps.Observer itself.
type turnScopedObserver struct {
	inner observe.Observer
	turns *turnTracer
}

// Start re-parents onto the open turn root when the caller's context is outside
// the turn's trace; otherwise it delegates verbatim.
func (o turnScopedObserver) Start(ctx context.Context, d observe.Descriptor) (context.Context, observe.Handle) {
	turn := o.turns.activeTurn()
	if turn == nil {
		return o.inner.Start(ctx, d)
	}
	if cur := observe.TraceCarrier(o.inner, ctx); cur != nil && traceIDOf(cur) == traceIDOf(turn) {
		return o.inner.Start(ctx, d)
	}
	return observe.StartFromCarrier(o.inner, ctx, turn, false, d)
}

// StartFromCarrier forwards verbatim: a caller that names its own carrier is
// expressing a parentage this wrapper must not second-guess.
func (o turnScopedObserver) StartFromCarrier(ctx context.Context, c *event.TraceCarrier, delayed bool, d observe.Descriptor) (context.Context, observe.Handle) {
	return observe.StartFromCarrier(o.inner, ctx, c, delayed, d)
}

// DecorateScope forwards verbatim: scope decoration is the inner observer's own
// vocabulary (a tenant, say) and is independent of which turn is open.
func (o turnScopedObserver) DecorateScope(ctx context.Context, d observe.Descriptor) context.Context {
	return observe.DecorateScope(o.inner, ctx, d)
}

// TraceCarrier reports the open turn's carrier as the active durable context, so
// events emitted during a turn correlate to that turn. This matters most on a
// RESUME: the resumed loop starts no fuse.loop.run span, so without this the
// agent's one-shot capture of its durable event carrier (agent.eventTrace, taken
// at Run entry) would find no active span and emit trace-less events — a
// regression against today. On a fresh start no turn is open at Run entry, so the
// captured carrier is still fuse.loop.run's, unchanged.
func (o turnScopedObserver) TraceCarrier(ctx context.Context) *event.TraceCarrier {
	if turn := o.turns.activeTurn(); turn != nil {
		return turn
	}
	return observe.TraceCarrier(o.inner, ctx)
}

// traceIDOf extracts the trace-id field of a W3C traceparent. It returns "" for a
// nil or malformed carrier (the carrier is validated before it ever reaches here,
// so "" means "no usable identity" and the comparison simply fails safe toward
// re-parenting).
func traceIDOf(c *event.TraceCarrier) string {
	if c == nil {
		return ""
	}
	parts := strings.Split(c.TraceParent, "-")
	if len(parts) != 4 {
		return ""
	}
	return parts[1]
}

// carrierFromEvents recovers the ORIGINAL session's durable trace context from a
// replayed stream: every event an agent emits carries the carrier captured at Run
// entry, so the first valid one is the session root a resumed loop's turn spans
// link back to (spec D2). Nil when the stream carries no usable context (an
// untraced binding), in which case turn roots are simply unlinked.
func carrierFromEvents(events []event.Event) *event.TraceCarrier {
	for _, e := range events {
		if e.Trace == nil || e.Trace.Validate() != nil {
			continue
		}
		c := *e.Trace
		return &c
	}
	return nil
}

// consumedTurns reports the highest fuse.turn.index the loop has already SPENT, so
// a resume continues the sequence rather than restarting it and colliding with the
// pre-reap turns of the same loop_id.
//
// Spent, not completed — that distinction is the whole point (review finding m2).
// Every COMPLETED interactive exchange ends in exactly one loop.parked event, but a
// session reaped MID-exchange opened (and teardown-ended) a turn span that never
// parked. Counting parks alone would hand that index straight back to the resumed
// session, producing two distinct fuse.loop.turn spans for one loop_id carrying the
// same fuse.turn.index — precisely the collision this seed exists to prevent, just
// reached down the mid-reap path instead of the parked one.
//
// So: count the parks, and add one more when the stream carries ANY event after the
// last park. The loop emits nothing at completion or teardown (see the run
// goroutine in launchLoop), so a trailing event can only be an exchange that started
// and never finished — the interrupted turn. An empty stream yields 0 and
// newTurnTracer floors that to 1, so the first resumed turn is index 2, the same
// index a first post-park turn would have taken.
func consumedTurns(events []event.Event) int {
	n := 0
	trailing := false
	for _, e := range events {
		if e.Kind == event.KindLoopParked {
			n++
			trailing = false
			continue
		}
		trailing = true
	}
	if trailing {
		// An exchange began after the last park and never reached one: its index is
		// spent even though nothing recorded its completion.
		n++
	}
	return n
}
