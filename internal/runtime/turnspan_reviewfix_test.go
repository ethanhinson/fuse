package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/event/fsstore"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/observe"
	"github.com/ethanhinson/fuse/internal/tools"
)

// TestTurnIndexSeedCountsAMidReapTurn is review finding m2. loop.parked is emitted
// only at a COMPLETED exchange, so a session reaped MID-exchange consumed a turn
// index (the tracer opened, and teardown ended, span N+1) without ever leaving a
// park behind. Seeding the resumed session from the park COUNT therefore re-issues
// that same index — two distinct fuse.loop.turn spans for one loop_id carrying the
// same fuse.turn.index, exactly the collision the seed exists to prevent.
//
// The seed must be the highest index the stream shows as CONSUMED: any event after
// the last park is an exchange that started and never completed.
func TestTurnIndexSeedCountsAMidReapTurn(t *testing.T) {
	parked := func(turn int) event.Event { return event.Event{Kind: event.KindLoopParked, Turn: turn} }

	cases := []struct {
		name   string
		events []event.Event
		want   int
	}{
		{
			name:   "empty stream",
			events: nil,
			want:   0,
		},
		{
			name:   "one completed exchange",
			events: []event.Event{{Kind: event.KindUserInput}, parked(0)},
			want:   1,
		},
		{
			name:   "two completed exchanges",
			events: []event.Event{{Kind: event.KindUserInput}, parked(0), {Kind: event.KindUserInput}, parked(1)},
			want:   2,
		},
		{
			// The mid-reap case: exchange 2 started (user input, a model call) and the
			// session died before it could park. Index 2 is spent.
			name: "reaped mid-exchange after one park",
			events: []event.Event{
				{Kind: event.KindUserInput}, parked(0),
				{Kind: event.KindUserInput}, {Kind: event.KindModelCallStart},
			},
			want: 2,
		},
		{
			// Reaped inside the FIRST exchange: index 1 (the one fuse.loop.run covers)
			// is spent, which is also what newTurnTracer's floor already assumes.
			name:   "reaped inside the first exchange",
			events: []event.Event{{Kind: event.KindUserInput}, {Kind: event.KindModelCallStart}},
			want:   1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := consumedTurns(tc.events); got != tc.want {
				t.Fatalf("consumedTurns = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestMidReapResumeDoesNotReuseATurnIndex is m2 end-to-end through the tracer: a
// resumed session seeded from a mid-reap stream must open its first turn ABOVE the
// index the reaped turn already used.
func TestMidReapResumeDoesNotReuseATurnIndex(t *testing.T) {
	// Pre-reap session: fuse.loop.run covers turn 1, one park, then turn 2 opened and
	// torn down by the reap without parking.
	preReap := &recordingObserver{}
	pre := newTurnTracer(preReap, context.Background(), nil, nil, 1)
	pre.wake() // index 2
	pre.teardown(observe.OutcomeCanceled)
	preIdx, ok := fieldValue(preReap.opsNamed("turn")[0], "fuse.turn.index")
	if !ok {
		t.Fatal("pre-reap turn span carried no fuse.turn.index")
	}

	// The stream that session left behind: one park, then an unparked exchange.
	events := []event.Event{
		{Kind: event.KindUserInput}, {Kind: event.KindLoopParked},
		{Kind: event.KindUserInput}, {Kind: event.KindModelCallStart},
	}

	post := &recordingObserver{}
	resumed := newTurnTracer(post, context.Background(), nil, nil, consumedTurns(events))
	resumed.wake()
	postIdx, ok := fieldValue(post.opsNamed("turn")[0], "fuse.turn.index")
	if !ok {
		t.Fatal("resumed turn span carried no fuse.turn.index")
	}
	if postIdx == preIdx {
		t.Fatalf("resumed session reused fuse.turn.index %q from the pre-reap mid-exchange turn", postIdx)
	}
}

// TestFailedResumeStillEmitsAnErrorSpan is review finding m4. A resume no longer
// starts fuse.loop.run (spec D4 forbids a second session root), and the turn tracer
// does not exist yet at the launch's early returns — so a resume that fails to
// build its agent used to vanish from traces and from the fuse_loop_* metrics
// entirely. A failed revival must stay a fact on the wire.
func TestFailedResumeStillEmitsAnErrorSpan(t *testing.T) {
	dir := t.TempDir()
	store := fsstore.NewDurableFSStore(dir)
	var reg event.LoopRegistry = store

	fakeA := newGatedCompleter(model.CompletionResp{Content: "first answer"})
	rtA := New(Deps{
		DurableStore:  store,
		Registry:      reg,
		MaxConcurrent: 1,
		IdleTTL:       120 * time.Millisecond,
		BuildAgent: func(s event.EventStore, tree *agent.AgentTree, modelID string, r *tools.Registry) (*agent.Agent, agent.ChildBuilder, string, error) {
			return agent.New(fakeA, execAll{r}, nopRenderer{}, modelID, "", 1, 0), nil, modelID, nil
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hA, err := rtA.StartLoop(ctx, LoopConfig{Task: "hello there", ModelID: "cloud/x", Interactive: true})
	if err != nil {
		t.Fatalf("A StartLoop: %v", err)
	}
	loopID := hA.ID()
	evA, unsubA, err := rtA.Observe(ctx, event.DefaultTenant, loopID)
	if err != nil {
		t.Fatalf("A Observe: %v", err)
	}
	fakeA.admit(0)
	waitForKind(t, evA, event.KindLoopParked, 2*time.Second)
	unsubA()
	select {
	case <-doneOf(hA):
	case <-time.After(2 * time.Second):
		t.Fatal("A's interactive loop was not reaped after going idle")
	}

	// Runtime B resumes, and its agent construction fails — the last early return in
	// launchLoop.
	buildErr := errors.New("boom")
	observer := &recordingObserver{}
	rtB := New(Deps{
		DurableStore:  store,
		Registry:      reg,
		MaxConcurrent: 1,
		Observer:      observer,
		IdleTTL:       time.Hour,
		BuildAgent: func(s event.EventStore, tree *agent.AgentTree, modelID string, r *tools.Registry) (*agent.Agent, agent.ChildBuilder, string, error) {
			return nil, nil, "", buildErr
		},
	})
	if _, err := rtB.Resume(ctx, event.DefaultTenant, loopID); !errors.Is(err, buildErr) {
		t.Fatalf("B Resume error = %v, want the build failure", err)
	}

	// Spec D4: a resume never mints a second fuse.loop.run.
	if runs := observer.opsNamed("run"); len(runs) != 0 {
		t.Fatalf("failed resume started %d fuse.loop.run spans, want 0: %+v", len(runs), runs)
	}
	resumes := observer.opsNamed("resume")
	if len(resumes) != 1 {
		t.Fatalf("failed resume started %d fuse.loop.resume spans, want exactly 1", len(resumes))
	}
	if resumes[0].kind != observe.OperationLoop {
		t.Errorf("loop.resume kind = %q, want %q", resumes[0].kind, observe.OperationLoop)
	}
	ends := observer.endsFor("resume")
	if len(ends) != 1 {
		t.Fatalf("loop.resume ended %d times, want exactly 1: %+v", len(ends), ends)
	}
	if ends[0].outcome != observe.OutcomeError {
		t.Errorf("failed resume outcome = %q, want %q", ends[0].outcome, observe.OutcomeError)
	}
}

// TestSuccessfulResumeEndsTheResumeSpanOnce pins the other half of m4: the launch
// span is short-lived — it ends with success once the launch is wired, and the run's
// own completion (which shares the endOnce guard) must not re-end or reopen it.
func TestSuccessfulResumeEndsTheResumeSpanOnce(t *testing.T) {
	dir := t.TempDir()
	store := fsstore.NewDurableFSStore(dir)
	var reg event.LoopRegistry = store

	fakeA := newGatedCompleter(model.CompletionResp{Content: "first answer"})
	rtA := New(Deps{
		DurableStore:  store,
		Registry:      reg,
		MaxConcurrent: 1,
		IdleTTL:       120 * time.Millisecond,
		BuildAgent: func(s event.EventStore, tree *agent.AgentTree, modelID string, r *tools.Registry) (*agent.Agent, agent.ChildBuilder, string, error) {
			return agent.New(fakeA, execAll{r}, nopRenderer{}, modelID, "", 1, 0), nil, modelID, nil
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hA, err := rtA.StartLoop(ctx, LoopConfig{Task: "hello there", ModelID: "cloud/x", Interactive: true})
	if err != nil {
		t.Fatalf("A StartLoop: %v", err)
	}
	loopID := hA.ID()
	evA, unsubA, err := rtA.Observe(ctx, event.DefaultTenant, loopID)
	if err != nil {
		t.Fatalf("A Observe: %v", err)
	}
	fakeA.admit(0)
	waitForKind(t, evA, event.KindLoopParked, 2*time.Second)
	unsubA()
	select {
	case <-doneOf(hA):
	case <-time.After(2 * time.Second):
		t.Fatal("A's interactive loop was not reaped after going idle")
	}

	observer := &recordingObserver{}
	fakeB := newGatedCompleter(model.CompletionResp{Content: "second answer"})
	rtB := New(Deps{
		DurableStore:  store,
		Registry:      reg,
		MaxConcurrent: 1,
		Observer:      observer,
		IdleTTL:       time.Hour,
		BuildAgent: func(s event.EventStore, tree *agent.AgentTree, modelID string, r *tools.Registry) (*agent.Agent, agent.ChildBuilder, string, error) {
			return agent.New(fakeB, execAll{r}, nopRenderer{}, modelID, "", 1, 0), nil, modelID, nil
		},
	})
	if _, err := rtB.Resume(ctx, event.DefaultTenant, loopID); err != nil {
		t.Fatalf("B Resume: %v", err)
	}

	waitFor(t, 2*time.Second, "the resume launch span to end", func() bool {
		return len(observer.endsFor("resume")) == 1
	})
	if runs := observer.opsNamed("run"); len(runs) != 0 {
		t.Fatalf("resume started %d fuse.loop.run spans, want 0: %+v", len(runs), runs)
	}
	ends := observer.endsFor("resume")
	if ends[0].outcome != observe.OutcomeSuccess {
		t.Errorf("successful resume launch outcome = %q, want %q", ends[0].outcome, observe.OutcomeSuccess)
	}
	// The resumed session's first turn still opens, and still above the pre-reap index.
	waitFor(t, 2*time.Second, "the resumed session's first turn span", func() bool {
		return len(observer.opsNamed("turn")) == 1
	})
	if v, ok := fieldValue(observer.opsNamed("turn")[0], "fuse.turn.index"); !ok || v != "2" {
		t.Errorf("resumed first turn fuse.turn.index = %q (present=%v), want \"2\"", v, ok)
	}
}
