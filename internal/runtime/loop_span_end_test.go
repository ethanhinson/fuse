package runtime

import (
	"context"
	"sync"
	"testing"

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

// TestLoopSpanShape: a run produces exactly one loop.run operation through the
// production StartLoop, carrying only the tenant attribute, ended exactly once.
func TestLoopSpanShape(t *testing.T) {
	observer := &recordingObserver{}
	fake := &scriptedCompleter{responses: []model.CompletionResp{{Content: "final answer"}}}
	rt := New(Deps{
		Observer:      observer,
		MaxConcurrent: 1,
		BuildAgent: func(_ event.EventStore, _ *agent.AgentTree, modelID string, reg *tools.Registry) (*agent.Agent, error) {
			return agent.New(fake, execAll{reg}, nopRenderer{}, modelID, "", 1, 0), nil
		},
	})

	h, err := rt.StartLoop(context.Background(), LoopConfig{Task: "hi", ModelID: "cloud/x"})
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	if _, err := h.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	runs := observer.opsNamed("run")
	if len(runs) != 1 {
		t.Fatalf("started %d loop.run operations, want exactly 1", len(runs))
	}
	if runs[0].kind != observe.OperationLoop {
		t.Errorf("loop.run kind = %q, want %q", runs[0].kind, observe.OperationLoop)
	}
	if len(runs[0].fields) != 1 {
		t.Fatalf("loop.run fields = %+v, want exactly the tenant attribute", runs[0].fields)
	}
	if v, ok := fieldValue(runs[0], "tenant"); !ok || v != string(event.DefaultTenant) {
		t.Errorf("loop.run tenant = %q (present=%v), want %q", v, ok, event.DefaultTenant)
	}
	ends := observer.endsFor("run")
	if len(ends) != 1 {
		t.Fatalf("loop.run ended %d times, want exactly 1: %+v", len(ends), ends)
	}
	if ends[0].outcome != observe.OutcomeSuccess {
		t.Errorf("loop.run outcome = %q, want %q", ends[0].outcome, observe.OutcomeSuccess)
	}
}
