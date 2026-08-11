package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/tools"
)

// newInteractiveRuntime builds a runtime whose agents are constructed with a FINITE
// maxTurns=1 — deliberately, to prove the runtime lifts the cap for an interactive
// loop (StartLoop calls SetMaxTurns(0)). This mirrors production: the loop-server
// BuildAgent bakes a finite headless backstop into the root agent, and interactive
// mode must override it so a multi-turn conversation is not truncated at the cap.
func newInteractiveRuntime(t *testing.T, fake agent.Completer) Runtime {
	t.Helper()
	return New(Deps{
		BaseDir:       t.TempDir(),
		MaxConcurrent: 1,
		BuildAgent: func(store event.EventStore, tree *agent.AgentTree, modelID string, reg *tools.Registry) (*agent.Agent, agent.ChildBuilder, string, error) {
			return agent.New(fake, execAll{reg}, nopRenderer{}, modelID, "", 1, 0), nil, modelID, nil // finite cap; runtime must lift it
		},
	})
}

// gatedCompleter returns queued responses in order, one per admitted token. Once a
// response is consumed it BLOCKS on the gate channel until the test admits the next
// one (or ctx is cancelled). This models a live model that only produces output when
// there is a turn to answer: an interactive loop parked in Wait must not call
// Complete again until a human message wakes it, so an accidental extra Complete
// blocks — surfacing as a timeout the test catches rather than silently spinning.
type gatedCompleter struct {
	responses []model.CompletionResp
	gate      chan int
}

func newGatedCompleter(resps ...model.CompletionResp) *gatedCompleter {
	return &gatedCompleter{responses: resps, gate: make(chan int, len(resps))}
}

// admit permits the next Complete call to proceed with response index i.
func (g *gatedCompleter) admit(i int) { g.gate <- i }

func (g *gatedCompleter) Complete(ctx context.Context, req model.CompletionReq) (model.CompletionResp, error) {
	select {
	case i := <-g.gate:
		if i >= 0 && i < len(g.responses) {
			return g.responses[i], nil
		}
		return model.CompletionResp{Content: "done"}, nil
	case <-ctx.Done():
		return model.CompletionResp{}, ctx.Err()
	}
}

// TestInteractiveLoopParksAndResumes proves the persistent conversational loop:
//   - loop.start with Interactive:true runs a first turn, answers (no tool calls),
//     and PARKS rather than finishing;
//   - Send on the SAME loop_id succeeds (no ErrLoopFinished) and wakes the loop;
//   - a second turn.end arrives on the same event stream — one loop_id, one stream.
func TestInteractiveLoopParksAndResumes(t *testing.T) {
	fake := newGatedCompleter(
		model.CompletionResp{Content: "first answer"},  // turn 0: no tools -> park
		model.CompletionResp{Content: "second answer"}, // turn 1 after Send -> park again
	)
	rt := newInteractiveRuntime(t, fake)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start with the model gated CLOSED so we can subscribe to the live tail BEFORE
	// the first turn produces any events (mirrors TestObserveAndAttach); otherwise the
	// first turn.end can flush before Observe subscribes and the live tail misses it.
	h, err := rt.StartLoop(ctx, LoopConfig{Task: "find me a rental", ModelID: "cloud/x", Interactive: true})
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	loopID := h.ID()

	evCh, unsub, err := rt.Observe(ctx, event.DefaultTenant, loopID)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	defer unsub()

	fake.admit(0) // now let the first turn's model call proceed
	// The loop emits a deterministic loop.parked signal at the terminal turn — the
	// "exchange complete, awaiting input" marker a conversational client keys on.
	waitForKind(t, evCh, event.KindLoopParked, 2*time.Second)

	// The loop is parked, not finished: Send must succeed (no ErrLoopFinished).
	fake.admit(1) // permit the follow-up answer once the loop wakes
	if err := rt.Send(ctx, event.DefaultTenant, loopID, "what about Aspen?"); err != nil {
		t.Fatalf("Send to a parked interactive loop should succeed, got: %v", err)
	}

	// A second loop.parked on the SAME stream proves the loop resumed under one
	// loop_id and parked again after answering the follow-up.
	waitForKind(t, evCh, event.KindLoopParked, 2*time.Second)

	// Cancel ends the parked loop; the run must return cleanly.
	cancel()
	select {
	case <-doneOf(h):
	case <-time.After(2 * time.Second):
		t.Fatal("interactive loop did not exit after ctx cancel")
	}
}

// TestNonInteractiveLoopStillFinishes guards the default: without Interactive, a
// no-tool answer finishes the loop and a later Send reports an error — byte-identical
// to the pre-change single-task contract.
func TestNonInteractiveLoopStillFinishes(t *testing.T) {
	fake := newGatedCompleter(model.CompletionResp{Content: "one and done"})
	rt := newTestRuntime(t, fake)

	fake.admit(0)
	h, err := rt.StartLoop(context.Background(), LoopConfig{Task: "hi", ModelID: "cloud/x"}) // Interactive:false
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	if _, err := h.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if err := rt.Send(context.Background(), event.DefaultTenant, h.ID(), "again?"); err == nil {
		t.Fatal("Send to a finished non-interactive loop should error, got nil")
	}
}

// waitForKind drains evCh until an event of kind k arrives or the deadline passes.
func waitForKind(t *testing.T, evCh <-chan event.Event, k event.Kind, d time.Duration) {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case ev, ok := <-evCh:
			if !ok {
				t.Fatalf("event channel closed before seeing %s", k)
			}
			if ev.Kind == k {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s", k)
		}
	}
}

// doneOf exposes a loop handle's completion as a channel for select.
func doneOf(h LoopHandle) <-chan struct{} {
	ch := make(chan struct{})
	go func() { _, _ = h.Wait(); close(ch) }()
	return ch
}
