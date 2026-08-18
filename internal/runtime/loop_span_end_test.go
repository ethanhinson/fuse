package runtime

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/observe"
	"github.com/ethanhinson/fuse/internal/tools"
)

// TestEndOnceIsFirstWriterWins pins the guard's documented semantics: the FIRST
// outcome handed to end is the one that reaches the span; every later call is
// dropped, outcome and all. Turn-scoped tracing depends on this — the park path
// ends loop.run with success, and a later cancellation must not overwrite it.
func TestEndOnceIsFirstWriterWins(t *testing.T) {
	o := &recordingObserver{}
	_, h := o.Start(context.Background(), observe.Descriptor{Kind: observe.OperationLoop, Name: "run"})
	end := endOnce(h)

	end(observe.OutcomeSuccess)
	end(observe.OutcomeError)
	end(observe.OutcomeCanceled)

	ends := o.endsFor("run")
	if len(ends) != 1 {
		t.Fatalf("endOnce forwarded %d End calls, want exactly 1: %+v", len(ends), ends)
	}
	if ends[0].outcome != observe.OutcomeSuccess {
		t.Errorf("first-writer-wins violated: End outcome = %q, want %q", ends[0].outcome, observe.OutcomeSuccess)
	}
}

// TestEndOnceUnderConcurrentCallers drives the loop-span end path from many
// goroutines at once and asserts exactly one End reaches the handle. Run under
// -race, this is the guard's real assertion: a plain `ended bool` both races on
// the flag and can forward two Ends. Task 4 wires a second, genuinely concurrent
// caller (the park callback on the run goroutine alongside teardown), so the guard
// must be safe before those callers exist.
func TestEndOnceUnderConcurrentCallers(t *testing.T) {
	for iter := 0; iter < 50; iter++ {
		o := &recordingObserver{}
		_, h := o.Start(context.Background(), observe.Descriptor{Kind: observe.OperationLoop, Name: "run"})
		end := endOnce(h)

		const callers = 8
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < callers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				end(observe.OutcomeSuccess)
			}()
		}
		close(start)
		wg.Wait()

		if ends := o.endsFor("run"); len(ends) != 1 {
			t.Fatalf("iteration %d: %d concurrent callers produced %d End calls, want exactly 1", iter, callers, len(ends))
		}
	}
}

// TestInteractiveLoopSpanEndsOnceUnderReapRace drives the guard through the REAL
// seam (learnings: runtime-deps-field-overwrites-builder-injection — assert through
// StartLoop, not the builder): an interactive loop parks, then an idle reap and a
// request-ctx cancellation race the run goroutine's completion path. However the
// race lands, fuse.loop.run must be started once and ended exactly once.
func TestInteractiveLoopSpanEndsOnceUnderReapRace(t *testing.T) {
	fake := newGatedCompleter(model.CompletionResp{Content: "only answer"})
	observer := &recordingObserver{}
	rt := New(Deps{
		BaseDir:       t.TempDir(),
		MaxConcurrent: 1,
		Observer:      observer,
		// Short enough that the reaper fires while the test is still cancelling the
		// request ctx, so teardown and completion genuinely overlap.
		IdleTTL: 50 * time.Millisecond,
		BuildAgent: func(store event.EventStore, tree *agent.AgentTree, modelID string, r *tools.Registry) (*agent.Agent, agent.ChildBuilder, string, error) {
			return agent.New(fake, execAll{r}, nopRenderer{}, modelID, "", 1, 0), nil, modelID, nil
		},
	})

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
	// Drop the live Observe so the session is genuinely idle and the reaper arms.
	unsub()

	// Race the client disconnect against the reaper-driven completion.
	go cancel()

	select {
	case <-doneOf(h):
	case <-time.After(3 * time.Second):
		t.Fatal("parked interactive loop was not reaped within the timeout")
	}

	if ops := observer.opsNamed("run"); len(ops) != 1 {
		t.Fatalf("started %d loop.run operations, want exactly 1", len(ops))
	}
	if ends := observer.endsFor("run"); len(ends) != 1 {
		t.Fatalf("loop.run ended %d times, want exactly 1: %+v", len(ends), ends)
	}
}
