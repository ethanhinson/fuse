package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/event/fsstore"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/observe"
	observeotel "github.com/ethanhinson/fuse/internal/observe/otel"
	"github.com/ethanhinson/fuse/internal/tools"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// This file covers the SPAN-TOPOLOGY findings of change 0071's review: the
// durable-store spans a loop emits are as much a part of the turn topology as the
// agent's own spans, and the resume path is where a session's loop-scope context is
// easiest to drop on the floor. Every test here pairs a real durable store with a
// parentage-aware observer double (markingObserver), because the defect class is
// invisible to an observer that records only descriptors.

// durableObservedRuntime builds a runtime whose loop emits into a real durable store
// (so durableSink — not fsstore — is the loop's event sink) under the given observer.
func durableObservedRuntime(t *testing.T, store *fsstore.FSDurableStore, fake agent.Completer, observer observe.Observer, idle time.Duration) Runtime {
	t.Helper()
	return New(Deps{
		DurableStore:  store,
		Registry:      store,
		MaxConcurrent: 1,
		Observer:      observer,
		IdleTTL:       idle,
		BuildAgent: func(s event.EventStore, tree *agent.AgentTree, modelID string, reg *tools.Registry) (*agent.Agent, agent.ChildBuilder, string, error) {
			return agent.New(fake, execAll{reg}, nopRenderer{}, modelID, "", 1, 0), nil, modelID, nil
		},
	})
}

// TestDurableStoreSpansParentToTheOpenTurn is the F-A1 regression. durableSink is
// constructed with its OWN observer, and it was handed the raw Deps.Observer plus a
// session context whose active span is fuse.loop.run — which now ENDS at the first
// park. Every fuse.store.append of turns 2..N therefore hung off an already-exported,
// already-ended root: the exact pathology this change exists to remove, displaced from
// agent spans onto store spans.
func TestDurableStoreSpansParentToTheOpenTurn(t *testing.T) {
	store := fsstore.NewDurableFSStore(t.TempDir())
	observer := newMarkingObserver()
	fake := newGatedCompleter(
		model.CompletionResp{Content: "first answer"},
		model.CompletionResp{Content: "second answer"},
	)
	rt := durableObservedRuntime(t, store, fake, observer, time.Hour)

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
		t.Fatalf("want exactly 1 fuse.loop.run, got %d", len(runIDs))
	}
	firstIDs, firstParents := observer.snapshot("append")
	if len(firstIDs) == 0 {
		t.Fatal("the first exchange emitted no store.append spans; the loop is not on the durable sink")
	}
	for i, p := range firstParents {
		if p != runIDs[0] {
			t.Errorf("first-exchange append #%d parent = %q, want the session root %q", i, p, runIDs[0])
		}
	}

	fake.admit(1)
	if err := rt.Send(ctx, event.DefaultTenant, h.ID(), "and then?"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitForKind(t, evCh, event.KindLoopParked, 2*time.Second)
	waitFor(t, 2*time.Second, "the woken exchange's store appends", func() bool {
		ids, _ := observer.snapshot("append")
		return len(ids) > len(firstIDs)
	})

	turnIDs, _ := observer.snapshot("turn")
	if len(turnIDs) != 1 {
		t.Fatalf("want exactly 1 fuse.loop.turn root, got %d", len(turnIDs))
	}
	ids, parents := observer.snapshot("append")
	for i := len(firstIDs); i < len(ids); i++ {
		if parents[i] != turnIDs[0] {
			t.Errorf("post-wake append #%d parent = %q, want the OPEN turn root %q — the durable sink is still "+
				"emitting under the ended fuse.loop.run", i, parents[i], turnIDs[0])
		}
	}
}

// reapedInteractiveSession drives ONE interactive exchange under observer, then lets
// the idle reaper finish the session, leaving a resumable durable stream behind. It is
// the fixture every resume-path test below shares: the whole point is what the SECOND
// process does with the stream the first one left.
func reapedInteractiveSession(t *testing.T, tenant event.TenantID, observer observe.Observer) (*fsstore.FSDurableStore, string) {
	t.Helper()
	store := fsstore.NewDurableFSStore(t.TempDir())
	fake := newGatedCompleter(model.CompletionResp{Content: "first answer"})
	rt := durableObservedRuntime(t, store, fake, observer, 120*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, err := rt.StartLoop(ctx, LoopConfig{Task: "hello there", ModelID: "cloud/x", Tenant: tenant, Interactive: true})
	if err != nil {
		t.Fatalf("seed StartLoop: %v", err)
	}
	evCh, unsub, err := rt.Observe(ctx, tenant, h.ID())
	if err != nil {
		t.Fatalf("seed Observe: %v", err)
	}
	fake.admit(0)
	waitForKind(t, evCh, event.KindLoopParked, 2*time.Second)
	unsub() // go idle so the reaper arms
	select {
	case <-doneOf(h):
	case <-time.After(3 * time.Second):
		t.Fatal("the seed session was never reaped")
	}
	return store, h.ID()
}

// TestResumedStoreSpansParentToTheResumedTurn is the F-A2(a) regression. A resume
// starts no fuse.loop.run, so the loop context it hands durableSink is the CALLER's
// request context: every append of a revived session was emitted either rootless or —
// worse — under the transient inbound request span. A resume is a turn boundary, so
// the honest parent is the resumed turn root.
func TestResumedStoreSpansParentToTheResumedTurn(t *testing.T) {
	const tenant = event.TenantID("acme")
	store, loopID := reapedInteractiveSession(t, tenant, newMarkingObserver())

	observerB := newMarkingObserver()
	fakeB := newGatedCompleter(model.CompletionResp{Content: "second answer"})
	rtB := durableObservedRuntime(t, store, fakeB, observerB, time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Model the loop server: Resume arrives INSIDE an inbound request span.
	reqCtx, _ := observerB.Start(ctx, observe.Descriptor{Kind: observe.OperationAPIRequest, Name: "resume"})

	if _, err := rtB.Resume(reqCtx, tenant, loopID); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	evB, unsubB, err := rtB.Observe(ctx, tenant, loopID)
	if err != nil {
		t.Fatalf("B Observe: %v", err)
	}
	defer unsubB()
	fakeB.admit(0)
	waitForKind(t, evB, event.KindLoopParked, 2*time.Second)

	turnIDs, _ := observerB.snapshot("turn")
	if len(turnIDs) != 1 {
		t.Fatalf("resume started %d fuse.loop.turn roots, want exactly 1", len(turnIDs))
	}
	reqIDs, _ := observerB.snapshot("resume")
	ids, parents := observerB.snapshot("append")
	if len(ids) == 0 {
		t.Fatal("the resumed exchange emitted no store.append spans")
	}
	for i, p := range parents {
		if p == reqIDs[0] {
			t.Errorf("resumed append #%d parented to the inbound request span %q; a session outlives the request that revived it", i, p)
		}
		if p != turnIDs[0] {
			t.Errorf("resumed append #%d parent = %q, want the resumed turn root %q", i, p, turnIDs[0])
		}
	}
}

// TestResumedLaunchKeepsTenantScope is the F-A2(b) regression, and the one with a
// production consequence beyond traces. The composition root's metrics observer stamps
// the tenant into the context AT LOOP START, from fuse.loop.run's descriptor, and every
// later Start/StartFromCarrier reads it back OUT of the context. Skipping the loop-scope
// decoration on resume made every model-attempt, tool, store and turn metric of a
// resumed session record tenant="" — change 0051's per-tenant series, silently
// mis-attributed for the whole life of the session. markingObserver models exactly that
// mechanism (DecorateScope stamps, every start reads).
func TestResumedLaunchKeepsTenantScope(t *testing.T) {
	const tenant = event.TenantID("acme")
	store, loopID := reapedInteractiveSession(t, tenant, newMarkingObserver())

	observerB := newMarkingObserver()
	fakeB := newGatedCompleter(model.CompletionResp{Content: "second answer"})
	rtB := durableObservedRuntime(t, store, fakeB, observerB, time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := rtB.Resume(ctx, tenant, loopID); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	evB, unsubB, err := rtB.Observe(ctx, tenant, loopID)
	if err != nil {
		t.Fatalf("B Observe: %v", err)
	}
	defer unsubB()
	fakeB.admit(0)
	waitForKind(t, evB, event.KindLoopParked, 2*time.Second)

	for _, name := range []string{"turn", "complete", "append"} {
		seen := observerB.scopesOf(name)
		if len(seen) == 0 {
			t.Fatalf("the resumed session started no %q operation", name)
		}
		for i, got := range seen {
			if got != string(tenant) {
				t.Errorf("resumed %s #%d observed tenant scope %q, want %q — the resumed launch dropped the loop-scope decoration",
					name, i, got, tenant)
			}
		}
	}
}

// TestResumeDoesNotLinkTurnRootsToTheCallerRequestSpan is the F-A3 regression. When the
// replayed stream carries no recoverable trace context (an untraced or pre-tracing
// original session), the fallback took the carrier of the RESUME CALLER's context — in
// the loop server, the inbound HTTP request span. That link looks authoritative, is
// session-unrelated, and differs on every Resume of the same loop_id. Unlinked is the
// honest outcome.
func TestResumeDoesNotLinkTurnRootsToTheCallerRequestSpan(t *testing.T) {
	const tenant = event.TenantID("acme")
	// The seed session is UNTRACED, so its durable events carry no carrier at all.
	store, loopID := reapedInteractiveSession(t, tenant, observe.NoopObserver{})

	// recordingObserver always reports a valid carrier for the caller's context — the
	// stand-in for the request span the resume arrives inside.
	observerB := &recordingObserver{}
	fakeB := newGatedCompleter(model.CompletionResp{Content: "second answer"})
	rtB := durableObservedRuntime(t, store, fakeB, observerB, time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := rtB.Resume(ctx, tenant, loopID); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	evB, unsubB, err := rtB.Observe(ctx, tenant, loopID)
	if err != nil {
		t.Fatalf("B Observe: %v", err)
	}
	defer unsubB()
	fakeB.admit(0)
	waitForKind(t, evB, event.KindLoopParked, 2*time.Second)

	turns := observerB.opsNamed("turn")
	if len(turns) != 1 {
		t.Fatalf("resume started %d turn roots, want exactly 1: %+v", len(turns), turns)
	}
	if turns[0].hadCarrier {
		t.Errorf("resumed turn root linked to carrier %q, but the replayed stream carried none — "+
			"the fallback linked the session to the transient resume caller's span", turns[0].carrier)
	}
}

// TestUnlinkedResumedTurnIsStillARoot is the F-A4 regression, and it is only reachable
// once F-A3 stops manufacturing a link. A turn started with a nil link fell through
// observe.StartFromCarrier to a plain Start, silently parenting the turn to whatever
// span the caller's context held — so it stopped being a ROOT at all, which defeats the
// entire change. Asserted through the REAL OTEL adapter and exporter, since "is a root"
// is a property of the provider, not of a double.
func TestUnlinkedResumedTurnIsStillARoot(t *testing.T) {
	const tenant = event.TenantID("acme")
	store, loopID := reapedInteractiveSession(t, tenant, observe.NoopObserver{})

	exporter := tracetest.NewInMemoryExporter()
	provider := observeotel.NewProvider(exporter, observeotel.BatchConfig{BatchSize: 1, BatchTimeout: time.Millisecond},
		sdktrace.WithSampler(sdktrace.AlwaysSample()))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	observerB := observeotel.New(provider)

	fakeB := newGatedCompleter(model.CompletionResp{Content: "second answer"})
	rtB := durableObservedRuntime(t, store, fakeB, observerB, time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reqCtx, reqSpan := observerB.Start(ctx, observe.Descriptor{Kind: observe.OperationAPIRequest, Name: "resume"})
	if _, err := rtB.Resume(reqCtx, tenant, loopID); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	evB, unsubB, err := rtB.Observe(ctx, tenant, loopID)
	if err != nil {
		t.Fatalf("B Observe: %v", err)
	}
	defer unsubB()
	fakeB.admit(0)
	waitForKind(t, evB, event.KindLoopParked, 2*time.Second)
	reqSpan.End(observe.OutcomeSuccess)
	flush(t, provider)

	requests := exportedNamed(t, exporter, "fuse.api.request.resume")
	if len(requests) != 1 {
		t.Fatalf("exported fuse.api.request.resume spans = %d, want 1", len(requests))
	}
	turns := exportedNamed(t, exporter, "fuse.loop.turn")
	if len(turns) != 1 {
		t.Fatalf("exported fuse.loop.turn spans = %d, want 1", len(turns))
	}
	if turns[0].Parent.IsValid() {
		t.Errorf("unlinked turn span is not a trace root: parent = %v", turns[0].Parent)
	}
	if turns[0].SpanContext.TraceID() == requests[0].SpanContext.TraceID() {
		t.Error("unlinked turn span joined the resume request's trace; it must be its own root trace")
	}
	if len(turns[0].Links) != 0 {
		t.Errorf("unlinked turn span carries links %v, want none — there was no session carrier to link to", turns[0].Links)
	}
}
