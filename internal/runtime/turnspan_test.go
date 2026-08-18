package runtime

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/event/fsstore"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/observe"
	"github.com/ethanhinson/fuse/internal/tools"
)

// newObservedRuntime builds a runtime over the supplied observer and completer.
// Every assertion in this file is reached through the REAL StartLoop/Resume path
// (learnings: runtime-deps-field-overwrites-builder-injection — the runtime is the
// later writer of Deps.Observer onto the agent, so a builder-level test proves
// nothing about the shipped wiring).
func newObservedRuntime(t *testing.T, fake agent.Completer, observer observe.Observer, idle time.Duration) Runtime {
	t.Helper()
	return New(Deps{
		BaseDir:       t.TempDir(),
		MaxConcurrent: 1,
		Observer:      observer,
		IdleTTL:       idle,
		BuildAgent: func(store event.EventStore, tree *agent.AgentTree, modelID string, reg *tools.Registry) (*agent.Agent, agent.ChildBuilder, string, error) {
			return agent.New(fake, execAll{reg}, nopRenderer{}, modelID, "", 1, 0), nil, modelID, nil
		},
	})
}

// waitFor polls cond until it holds or the deadline passes. Turn spans are started
// and ended on the RUN goroutine at the park/wake boundary, which is not ordered
// against the event a test observes, so assertions on them poll rather than assume.
func waitFor(t *testing.T, d time.Duration, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", msg)
}

// TestOneShotLoopSpanShapeIsUnchanged is the GATE for change 0071: every turn-span
// code path is behind `interactive`, so a one-shot run must produce exactly the span
// shape it produced before — one fuse.loop.run started through the plain Start path
// (not the carrier path), carrying only the tenant attribute, ended exactly once, and
// no fuse.loop.turn at all. Asserted against the real production span emitted through
// StartLoop, not a shared fixture (learnings:
// parity-test-feeds-each-side-its-own-production-source).
func TestOneShotLoopSpanShapeIsUnchanged(t *testing.T) {
	observer := &recordingObserver{}
	fake := &scriptedCompleter{responses: []model.CompletionResp{{Content: "final answer"}}}
	rt := newObservedRuntime(t, fake, observer, 0)

	h, err := rt.StartLoop(context.Background(), LoopConfig{Task: "hi", ModelID: "cloud/x"}) // Interactive:false
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	if _, err := h.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	runs := observer.opsNamed("run")
	if len(runs) != 1 {
		t.Fatalf("one-shot started %d loop.run operations, want exactly 1", len(runs))
	}
	if runs[0].kind != observe.OperationLoop {
		t.Errorf("loop.run kind = %q, want %q", runs[0].kind, observe.OperationLoop)
	}
	if runs[0].viaCarrier {
		t.Error("one-shot loop.run went through StartFromCarrier; it must keep using plain Start")
	}
	if len(runs[0].fields) != 1 {
		t.Fatalf("one-shot loop.run fields = %+v, want exactly the tenant attribute", runs[0].fields)
	}
	if v, ok := fieldValue(runs[0], "tenant"); !ok || v != string(event.DefaultTenant) {
		t.Errorf("loop.run tenant = %q (present=%v), want %q", v, ok, event.DefaultTenant)
	}
	if turns := observer.opsNamed("turn"); len(turns) != 0 {
		t.Errorf("one-shot started %d loop.turn operations, want 0: %+v", len(turns), turns)
	}
	ends := observer.endsFor("run")
	if len(ends) != 1 {
		t.Fatalf("one-shot loop.run ended %d times, want exactly 1: %+v", len(ends), ends)
	}
	if ends[0].outcome != observe.OutcomeSuccess {
		t.Errorf("one-shot loop.run outcome = %q, want %q", ends[0].outcome, observe.OutcomeSuccess)
	}
}

// TestFirstParkEndsLoopRunWhileRunIsAlive proves the change's headline behavior: the
// session root span closes (and therefore exports) at the FIRST park, while the run
// goroutine is still parked and alive — not hours later when the session is finally
// reaped.
func TestFirstParkEndsLoopRunWhileRunIsAlive(t *testing.T) {
	observer := &recordingObserver{}
	fake := newGatedCompleter(model.CompletionResp{Content: "first answer"})
	rt := newObservedRuntime(t, fake, observer, time.Hour) // no reap during this test

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h, err := rt.StartLoop(ctx, LoopConfig{Task: "hi", ModelID: "cloud/x", Interactive: true})
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	evCh, unsub, err := rt.Observe(ctx, event.DefaultTenant, h.ID())
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	defer unsub()

	fake.admit(0)
	waitForKind(t, evCh, event.KindLoopParked, 2*time.Second)
	waitFor(t, 2*time.Second, "loop.run to end at the first park", func() bool {
		return len(observer.endsFor("run")) == 1
	})

	// The run goroutine must still be alive: the span ended, the session did not.
	select {
	case <-doneOf(h):
		t.Fatal("the run goroutine completed; loop.run must end at the park with the run still parked")
	default:
	}
	if ends := observer.endsFor("run"); ends[0].outcome != observe.OutcomeSuccess {
		t.Errorf("loop.run park outcome = %q, want %q", ends[0].outcome, observe.OutcomeSuccess)
	}
}

// TestLaterTurnsAreLinkedRoots proves D3: each post-park exchange opens its own
// fuse.loop.turn ROOT span, started through the carrier path with delayed=true (new
// root + link back to the session trace) and carrying loop_id, the normalized tenant,
// and a 1-based fuse.turn.index whose 1 is the turn inside loop.run — so the first
// turn root is index 2 and the next is 3.
func TestLaterTurnsAreLinkedRoots(t *testing.T) {
	observer := &recordingObserver{}
	fake := newGatedCompleter(
		model.CompletionResp{Content: "first answer"},
		model.CompletionResp{Content: "second answer"},
		model.CompletionResp{Content: "third answer"},
	)
	rt := newObservedRuntime(t, fake, observer, time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h, err := rt.StartLoop(ctx, LoopConfig{Task: "hi", ModelID: "cloud/x", Interactive: true})
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	loopID := h.ID()
	evCh, unsub, err := rt.Observe(ctx, event.DefaultTenant, loopID)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	defer unsub()

	fake.admit(0)
	waitForKind(t, evCh, event.KindLoopParked, 2*time.Second)
	if turns := observer.opsNamed("turn"); len(turns) != 0 {
		t.Fatalf("the first exchange must ride loop.run, but %d turn spans exist: %+v", len(turns), turns)
	}

	for i, want := range []string{"2", "3"} {
		fake.admit(i + 1)
		if err := rt.Send(ctx, event.DefaultTenant, loopID, "again?"); err != nil {
			t.Fatalf("Send %d: %v", i, err)
		}
		waitForKind(t, evCh, event.KindLoopParked, 2*time.Second)

		idx := i + 1
		waitFor(t, 2*time.Second, fmt.Sprintf("turn span %d to start", idx), func() bool {
			return len(observer.opsNamed("turn")) == idx
		})
		op := observer.opsNamed("turn")[i]
		if op.kind != observe.OperationLoop {
			t.Errorf("turn %s kind = %q, want %q", want, op.kind, observe.OperationLoop)
		}
		if !op.viaCarrier {
			t.Errorf("turn %s was not started through observe.StartFromCarrier", want)
		}
		if !op.delayed {
			t.Errorf("turn %s started with delayed=false; a turn root must be a linked new root", want)
		}
		if !op.hadCarrier {
			t.Errorf("turn %s started with a nil carrier; the session carrier must survive the park", want)
		}
		if v, ok := fieldValue(op, "loop_id"); !ok || v != loopID {
			t.Errorf("turn %s loop_id = %q (present=%v), want %q", want, v, ok, loopID)
		}
		if v, ok := fieldValue(op, "tenant"); !ok || v != string(event.DefaultTenant) {
			t.Errorf("turn %s tenant = %q (present=%v), want %q", want, v, ok, event.DefaultTenant)
		}
		if v, ok := fieldValue(op, "fuse.turn.index"); !ok || v != want {
			t.Errorf("turn index = %q (present=%v), want %q", v, ok, want)
		}
	}
}

// TestTurnSpanEndsAtTheNextPark proves a turn root is closed (and exported) at the
// park that terminates its exchange, with the same outcome derivation loop.run's
// park end uses.
func TestTurnSpanEndsAtTheNextPark(t *testing.T) {
	observer := &recordingObserver{}
	fake := newGatedCompleter(
		model.CompletionResp{Content: "first answer"},
		model.CompletionResp{Content: "second answer"},
	)
	rt := newObservedRuntime(t, fake, observer, time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h, err := rt.StartLoop(ctx, LoopConfig{Task: "hi", ModelID: "cloud/x", Interactive: true})
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	evCh, unsub, err := rt.Observe(ctx, event.DefaultTenant, h.ID())
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	defer unsub()

	fake.admit(0)
	waitForKind(t, evCh, event.KindLoopParked, 2*time.Second)

	fake.admit(1)
	if err := rt.Send(ctx, event.DefaultTenant, h.ID(), "and then?"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitForKind(t, evCh, event.KindLoopParked, 2*time.Second)

	waitFor(t, 2*time.Second, "the second turn's span to end at its park", func() bool {
		return len(observer.endsFor("turn")) == 1
	})
	ends := observer.endsFor("turn")
	if ends[0].outcome != observe.OutcomeSuccess {
		t.Errorf("turn park outcome = %q, want %q", ends[0].outcome, observe.OutcomeSuccess)
	}
	// The exchange is over but the session is not: the run goroutine is parked again.
	select {
	case <-doneOf(h):
		t.Fatal("the run goroutine completed; it must be parked awaiting the next Send")
	default:
	}
}

// TestResumeEmitsLinkedTurnRootWithoutRestartingLoopRun proves the resume path (D4):
// a revived session is a CONTINUATION, so it opens a linked fuse.loop.turn root
// seeded from the restored stream and does NOT mint a second fuse.loop.run root for
// the same loop_id.
func TestResumeEmitsLinkedTurnRootWithoutRestartingLoopRun(t *testing.T) {
	dir := t.TempDir()
	store := fsstore.NewDurableFSStore(dir)
	var reg event.LoopRegistry = store

	// Runtime A drives TWO exchanges — enough that a resumed index seeded from the
	// stream (3) is distinguishable from one that merely restarts after loop.run (2) —
	// then is idle-reaped so the loop is finished.
	fakeA := newGatedCompleter(
		model.CompletionResp{Content: "first answer"},
		model.CompletionResp{Content: "second answer"},
	)
	rtA := New(Deps{
		DurableStore:  store,
		Registry:      reg,
		MaxConcurrent: 1,
		Observer:      &recordingObserver{},
		IdleTTL:       120 * time.Millisecond,
		BuildAgent: func(s event.EventStore, tree *agent.AgentTree, modelID string, r *tools.Registry) (*agent.Agent, agent.ChildBuilder, string, error) {
			return agent.New(fakeA, execAll{r}, nopRenderer{}, modelID, "", 1, 0), nil, modelID, nil
		},
	})
	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	hA, err := rtA.StartLoop(ctxA, LoopConfig{Task: "hello there", ModelID: "cloud/x", Interactive: true})
	if err != nil {
		t.Fatalf("A StartLoop: %v", err)
	}
	loopID := hA.ID()
	evA, unsubA, err := rtA.Observe(ctxA, event.DefaultTenant, loopID)
	if err != nil {
		t.Fatalf("A Observe: %v", err)
	}
	fakeA.admit(0)
	waitForKind(t, evA, event.KindLoopParked, 2*time.Second)
	fakeA.admit(1)
	if err := rtA.Send(ctxA, event.DefaultTenant, loopID, "and then?"); err != nil {
		t.Fatalf("A Send: %v", err)
	}
	waitForKind(t, evA, event.KindLoopParked, 2*time.Second)
	unsubA()
	select {
	case <-doneOf(hA):
	case <-time.After(2 * time.Second):
		t.Fatal("A's interactive loop was not reaped after going idle")
	}

	// Runtime B: a cold instance with its OWN observer, so every operation it records
	// belongs to the resumed launch. Its local durable context is deliberately DIFFERENT
	// from A's, so a resume that quietly fell back to its own context instead of
	// restoring the original session's carrier is visible rather than indistinguishable.
	observerB := distinctCarrierObserver{recordingObserver: &recordingObserver{}}
	fakeB := newGatedCompleter(model.CompletionResp{Content: "second answer"})
	rtB := New(Deps{
		DurableStore:  store,
		Registry:      reg,
		MaxConcurrent: 1,
		Observer:      observerB,
		IdleTTL:       time.Hour,
		BuildAgent: func(s event.EventStore, tree *agent.AgentTree, modelID string, r *tools.Registry) (*agent.Agent, agent.ChildBuilder, string, error) {
			return agent.New(fakeB, execAll{r}, nopRenderer{}, modelID, "", 1, 0), nil, modelID, nil
		},
	})
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()

	// Resume with the model gated CLOSED, subscribe, then admit — so the resumed
	// exchange's park cannot flush before the live tail exists.
	hB, err := rtB.Resume(ctxB, event.DefaultTenant, loopID)
	if err != nil {
		t.Fatalf("B Resume: %v", err)
	}
	evB, unsubB, err := rtB.Observe(ctxB, event.DefaultTenant, loopID)
	if err != nil {
		t.Fatalf("B Observe: %v", err)
	}
	defer unsubB()
	fakeB.admit(0)
	waitForKind(t, evB, event.KindLoopParked, 2*time.Second)

	if runs := observerB.opsNamed("run"); len(runs) != 0 {
		t.Errorf("resume started %d loop.run operations, want 0 — a resume continues the session, it does not restart it", len(runs))
	}
	turns := observerB.opsNamed("turn")
	if len(turns) != 1 {
		t.Fatalf("resume started %d loop.turn operations, want exactly 1: %+v", len(turns), turns)
	}
	if !turns[0].viaCarrier || !turns[0].delayed || !turns[0].hadCarrier {
		t.Errorf("resumed turn root: viaCarrier=%v delayed=%v hadCarrier=%v, want all true (a linked new root off the RESTORED carrier)",
			turns[0].viaCarrier, turns[0].delayed, turns[0].hadCarrier)
	}
	if turns[0].carrier != testCarrier.TraceParent {
		t.Errorf("resumed turn linked to carrier %q, want the ORIGINAL session's %q recovered from the replayed stream",
			turns[0].carrier, testCarrier.TraceParent)
	}
	if v, ok := fieldValue(turns[0], "loop_id"); !ok || v != loopID {
		t.Errorf("resumed turn loop_id = %q (present=%v), want %q", v, ok, loopID)
	}
	// Two exchanges completed before the reap, so the revived session continues the
	// sequence at 3 rather than restarting and colliding with the pre-reap turns of
	// this same loop_id.
	if v, ok := fieldValue(turns[0], "fuse.turn.index"); !ok || v != "3" {
		t.Errorf("resumed turn index = %q (present=%v), want \"3\" (seeded from the two completed exchanges in the replayed stream)", v, ok)
	}
	if hB.ID() != loopID {
		t.Fatalf("resumed loop id = %q, want %q", hB.ID(), loopID)
	}
}

// otherTraceParent is a second valid W3C traceparent, distinct from testCarrier's.
const otherTraceParent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

// distinctCarrierObserver is a recordingObserver whose OWN durable context differs
// from the one A's durable events carry. It exists so "the resumed loop linked to
// the RESTORED carrier" is a falsifiable claim rather than a tautology.
type distinctCarrierObserver struct{ *recordingObserver }

func (distinctCarrierObserver) TraceCarrier(context.Context) *event.TraceCarrier {
	return &event.TraceCarrier{TraceParent: otherTraceParent}
}

// spanCounts totals starts and ends across the two loop-lifecycle span names.
func spanCounts(o *recordingObserver) (starts, ends int) {
	for _, name := range []string{"run", "turn"} {
		starts += len(o.opsNamed(name))
		ends += len(o.endsFor(name))
	}
	return starts, ends
}

// TestReapLeavesNoOpenSpan is the leak guard: however a session ends — reaped while
// parked, or reaped mid-exchange with a turn span still open — teardown closes every
// span it opened. Run under -race, since teardown races the run goroutine.
func TestReapLeavesNoOpenSpan(t *testing.T) {
	t.Run("reaped while parked", func(t *testing.T) {
		observer := &recordingObserver{}
		fake := newGatedCompleter(model.CompletionResp{Content: "only answer"})
		rt := newObservedRuntime(t, fake, observer, 60*time.Millisecond)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		h, err := rt.StartLoop(ctx, LoopConfig{Task: "hi", ModelID: "cloud/x", Interactive: true})
		if err != nil {
			t.Fatalf("StartLoop: %v", err)
		}
		evCh, unsub, err := rt.Observe(ctx, event.DefaultTenant, h.ID())
		if err != nil {
			t.Fatalf("Observe: %v", err)
		}
		fake.admit(0)
		waitForKind(t, evCh, event.KindLoopParked, 2*time.Second)
		unsub() // go idle so the reaper arms

		select {
		case <-doneOf(h):
		case <-time.After(3 * time.Second):
			t.Fatal("parked interactive loop was not reaped within the timeout")
		}
		if starts, ends := spanCounts(observer); starts != ends {
			t.Fatalf("%d loop spans started but %d ended — a span leaked past teardown", starts, ends)
		}
	})

	t.Run("reaped mid-exchange with a turn span open", func(t *testing.T) {
		observer := &recordingObserver{}
		// Only the first response is ever admitted: the woken second exchange blocks in
		// Complete until the reaper cancels the session, so the reap lands with the turn
		// span still open.
		fake := newGatedCompleter(model.CompletionResp{Content: "only answer"})
		rt := newObservedRuntime(t, fake, observer, 80*time.Millisecond)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		h, err := rt.StartLoop(ctx, LoopConfig{Task: "hi", ModelID: "cloud/x", Interactive: true})
		if err != nil {
			t.Fatalf("StartLoop: %v", err)
		}
		evCh, unsub, err := rt.Observe(ctx, event.DefaultTenant, h.ID())
		if err != nil {
			t.Fatalf("Observe: %v", err)
		}
		fake.admit(0)
		waitForKind(t, evCh, event.KindLoopParked, 2*time.Second)
		unsub()

		if err := rt.Send(ctx, event.DefaultTenant, h.ID(), "and then?"); err != nil {
			t.Fatalf("Send: %v", err)
		}
		waitFor(t, 2*time.Second, "the woken exchange's turn span to open", func() bool {
			return len(observer.opsNamed("turn")) == 1
		})

		select {
		case <-doneOf(h):
		case <-time.After(3 * time.Second):
			t.Fatal("mid-exchange interactive loop was not reaped within the timeout")
		}
		if starts, ends := spanCounts(observer); starts != ends {
			t.Fatalf("%d loop spans started but %d ended — the open turn span leaked past teardown", starts, ends)
		}
	})
}

// markKey is the marking observer's private context key. It holds the started
// span's W3C traceparent, mirroring how a real provider carries span identity in
// the context.
type markKey struct{}

// markingObserver models the ONE property recordingObserver cannot: parentage. It
// behaves like the otel observer in the two ways that matter here — it stamps the
// span it starts into the context it returns, and it honors an explicit carrier
// (continuing that trace when delayed is false, opening a new root when it is
// true) — so a test can assert which span a later operation nested under without
// importing a telemetry vendor.
type markingObserver struct {
	mu      sync.Mutex
	n       int
	parents map[string][]string // op name -> parent id per start, in order
	ids     map[string][]string // op name -> own id per start, in order
	// byParent maps a minted traceparent back to the span id it identifies, so a
	// carrier-based parent resolves to a readable name.
	byParent map[string]string
}

func newMarkingObserver() *markingObserver {
	return &markingObserver{parents: map[string][]string{}, ids: map[string][]string{}, byParent: map[string]string{}}
}

// start records one span under the given parent id and returns a context carrying
// the new span's traceparent. traceHex threads the trace id through, so a
// re-parented span joins its new parent's trace (which is what makes
// turnScopedObserver's "already inside this trace?" check meaningful).
func (o *markingObserver) start(ctx context.Context, d observe.Descriptor, parent, traceHex string) (context.Context, observe.Handle) {
	o.mu.Lock()
	o.n++
	id := fmt.Sprintf("%s#%d", d.Name, o.n)
	if traceHex == "" {
		traceHex = fmt.Sprintf("%032x", o.n)
	}
	tp := fmt.Sprintf("00-%s-%016x-01", traceHex, o.n)
	o.parents[d.Name] = append(o.parents[d.Name], parent)
	o.ids[d.Name] = append(o.ids[d.Name], id)
	o.byParent[tp] = id
	o.mu.Unlock()
	return context.WithValue(ctx, markKey{}, tp), observe.NoopHandle{}
}

// parentOf resolves the span a context is currently inside, plus its trace id.
func (o *markingObserver) parentOf(ctx context.Context) (id, traceHex string) {
	tp, _ := ctx.Value(markKey{}).(string)
	return o.resolve(tp)
}

func (o *markingObserver) resolve(tp string) (id, traceHex string) {
	if tp == "" {
		return "", ""
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.byParent[tp], traceIDOf(&event.TraceCarrier{TraceParent: tp})
}

func (o *markingObserver) Start(ctx context.Context, d observe.Descriptor) (context.Context, observe.Handle) {
	parent, traceHex := o.parentOf(ctx)
	return o.start(ctx, d, parent, traceHex)
}

func (o *markingObserver) StartFromCarrier(ctx context.Context, c *event.TraceCarrier, delayed bool, d observe.Descriptor) (context.Context, observe.Handle) {
	if c == nil {
		return o.Start(ctx, d)
	}
	if delayed {
		return o.start(ctx, d, "", "") // a NEW root, merely linked to the carrier
	}
	parent, traceHex := o.resolve(c.TraceParent)
	return o.start(ctx, d, parent, traceHex) // a real child of the carrier's span
}

// TraceCarrier reports the context's own active span, exactly as the otel adapter
// does — which is what lets turnScopedObserver tell "already inside the turn" from
// "still pointing at the ended session root".
func (o *markingObserver) TraceCarrier(ctx context.Context) *event.TraceCarrier {
	tp, _ := ctx.Value(markKey{}).(string)
	if tp == "" {
		return nil
	}
	return &event.TraceCarrier{TraceParent: tp}
}

func (o *markingObserver) snapshot(name string) (ids, parents []string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.ids[name]...), append([]string(nil), o.parents[name]...)
}

// principalKey stands in for the composition root's per-loop identity decoration
// (Deps.LoopContext ⇒ toolidentity.WithPrincipal).
type principalKey struct{}

// TestReparentedSpanKeepsCallerContext is the safety constraint on the parenting
// mechanism. The re-parent must move only the span's PARENT: the context handed
// back to the agent still drives the model call and every tool Execute, so it must
// keep the caller's cancellation and values — above all the per-loop principal
// (change #59) the MCP egress mints delegation tokens from. Re-parenting by
// swapping in a stored turn context would silently strip both; re-parenting through
// the carrier does not.
func TestReparentedSpanKeepsCallerContext(t *testing.T) {
	inner := newMarkingObserver()
	sessionCtx, _ := inner.Start(context.Background(), observe.Descriptor{Kind: observe.OperationLoop, Name: "run"})
	turns := newTurnTracer(inner, sessionCtx, inner.TraceCarrier(sessionCtx), nil, 1)
	turns.wake()

	runCtx, cancel := context.WithCancel(context.WithValue(sessionCtx, principalKey{}, "alice"))
	defer cancel()
	obs := turnScopedObserver{inner: inner, turns: turns}
	spanCtx, _ := obs.Start(runCtx, observe.Descriptor{Kind: observe.OperationModelAttempt, Name: "complete"})

	if got, _ := spanCtx.Value(principalKey{}).(string); got != "alice" {
		t.Errorf("re-parented span context principal = %q, want %q: re-parenting must not drop caller values", got, "alice")
	}
	cancel()
	select {
	case <-spanCtx.Done():
	default:
		t.Error("re-parented span context did not inherit the caller's cancellation")
	}
	// And the re-parent really happened: the span nested under the turn, not the run.
	_, parents := inner.snapshot("complete")
	turnIDs, _ := inner.snapshot("turn")
	if len(parents) != 1 || parents[0] != turnIDs[0] {
		t.Fatalf("model attempt parents = %v, want [%s]", parents, turnIDs[0])
	}
}

// TestChildSpansParentToTheOpenTurn proves the parenting half of the design: work the
// agent performs after a wake (its model attempts) nests under the CURRENT turn root,
// not under the already-ended session root — while the first exchange still nests
// under loop.run.
func TestChildSpansParentToTheOpenTurn(t *testing.T) {
	observer := newMarkingObserver()
	fake := newGatedCompleter(
		model.CompletionResp{Content: "first answer"},
		model.CompletionResp{Content: "second answer"},
	)
	rt := newObservedRuntime(t, fake, observer, time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, err := rt.StartLoop(ctx, LoopConfig{Task: "hi", ModelID: "cloud/x", Interactive: true})
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	evCh, unsub, err := rt.Observe(ctx, event.DefaultTenant, h.ID())
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	defer unsub()

	fake.admit(0)
	waitForKind(t, evCh, event.KindLoopParked, 2*time.Second)

	runIDs, _ := observer.snapshot("run")
	if len(runIDs) != 1 {
		t.Fatalf("want exactly 1 loop.run, got %d", len(runIDs))
	}
	_, completeParents := observer.snapshot("complete")
	if len(completeParents) != 1 || completeParents[0] != runIDs[0] {
		t.Fatalf("first exchange's model attempt parents = %v, want [%s]", completeParents, runIDs[0])
	}

	fake.admit(1)
	if err := rt.Send(ctx, event.DefaultTenant, h.ID(), "and then?"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitForKind(t, evCh, event.KindLoopParked, 2*time.Second)
	waitFor(t, 2*time.Second, "the second exchange's model attempt", func() bool {
		_, parents := observer.snapshot("complete")
		return len(parents) == 2
	})

	turnIDs, _ := observer.snapshot("turn")
	if len(turnIDs) != 1 {
		t.Fatalf("want exactly 1 loop.turn, got %d", len(turnIDs))
	}
	_, completeParents = observer.snapshot("complete")
	if completeParents[1] != turnIDs[0] {
		t.Errorf("second exchange's model attempt parent = %q, want the open turn root %q", completeParents[1], turnIDs[0])
	}
}
