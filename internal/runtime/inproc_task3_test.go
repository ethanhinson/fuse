package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/tools"
)

// blockingCompleter blocks in Complete until release is closed, then returns a
// single no-tool-call assistant message. It lets a test subscribe (Observe) before
// the run produces its events.
type blockingCompleter struct {
	release <-chan struct{}
}

func (b blockingCompleter) Complete(ctx context.Context, req model.CompletionReq) (model.CompletionResp, error) {
	<-b.release
	return model.CompletionResp{Content: "ok"}, nil
}

func newTestRuntime(t *testing.T, fake agent.Completer) Runtime {
	t.Helper()
	return New(Deps{
		BaseDir:       t.TempDir(),
		MaxConcurrent: 2,
		BuildAgent: func(store event.EventStore, tree *agent.AgentTree, modelID string, reg *tools.Registry) (*agent.Agent, agent.ChildBuilder, string, error) {
			return agent.New(fake, execAll{reg}, nopRenderer{}, modelID, "", 1, 0), nil, modelID, nil
		},
	})
}

func TestObserveAndAttach(t *testing.T) {
	release := make(chan struct{})
	rt := newTestRuntime(t, blockingCompleter{release: release})

	h, err := rt.StartLoop(context.Background(), LoopConfig{Task: "hi", ModelID: "cloud/x"})
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}

	// Subscribe BEFORE the loop produces events (the completer is blocked).
	ch, cancel, err := rt.Observe(h.ID())
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	defer cancel()

	// Now let the run proceed; a live turn.start must arrive on the channel.
	close(release)

	var sawTurnStart bool
	for ev := range ch {
		if ev.Kind == event.KindTurnStart {
			sawTurnStart = true
			break
		}
	}
	if !sawTurnStart {
		t.Fatal("Observe channel never delivered a turn.start")
	}

	if _, err := h.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	// Attach returns durable history from Seq 0.
	evs, err := rt.Attach(h.ID(), 0)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if !hasKind(evs, event.KindTurnStart) || !hasKind(evs, event.KindTurnEnd) {
		t.Errorf("Attach history missing turn events: %v", evs)
	}
}

// TestSendEnqueuesForRoot proves Send to a RUNNING loop enqueues on the root node's
// human-bus queue (ADR-0022). The completer blocks before the first turn, so the run
// goroutine is live but its injector has not drained yet — the enqueue is observable.
func TestSendEnqueuesForRoot(t *testing.T) {
	release := make(chan struct{})
	rt := newTestRuntime(t, blockingCompleter{release: release})

	h, err := rt.StartLoop(context.Background(), LoopConfig{Task: "hi", ModelID: "cloud/x"})
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	// The loop is running (completer blocked): Send must enqueue and return nil.
	if err := rt.Send(context.Background(), h.ID(), "more work"); err != nil {
		t.Fatalf("Send to running loop: %v", err)
	}

	// The message is enqueued on the root node's human-bus queue (ADR-0022).
	// New returns the Runtime interface; reach the concrete loop state under test.
	lp := rt.(*inProcRuntime).loops[h.ID()]
	pending := lp.humanBus.Pending(h.ID())
	if len(pending) != 1 || pending[0].Text != "more work" {
		t.Fatalf("root queue = %+v, want one 'more work' message", pending)
	}

	// Let the run finish so the test doesn't leak the goroutine.
	close(release)
	if _, err := h.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}

// TestSendToFinishedLoopReturnsErrLoopFinished proves Send to a loop whose run
// goroutine has already completed returns the distinguishable ErrLoopFinished sentinel
// rather than silently stranding the input on a queue nothing drains.
func TestSendToFinishedLoopReturnsErrLoopFinished(t *testing.T) {
	fake := &scriptedCompleter{responses: []model.CompletionResp{{Content: "done"}}}
	rt := newTestRuntime(t, fake)

	h, err := rt.StartLoop(context.Background(), LoopConfig{Task: "hi", ModelID: "cloud/x"})
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	if _, err := h.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	if err := rt.Send(context.Background(), h.ID(), "more work"); !errors.Is(err, ErrLoopFinished) {
		t.Fatalf("Send to finished loop err = %v, want ErrLoopFinished", err)
	}

	// Nothing was enqueued: the finished-loop path returns before Enqueue.
	lp := rt.(*inProcRuntime).loops[h.ID()]
	if pending := lp.humanBus.Pending(h.ID()); len(pending) != 0 {
		t.Fatalf("finished loop should not enqueue; queue = %+v", pending)
	}
}

// TestObserveChannelClosesOnLoopCompletion proves FIX 3: when the loop's run
// goroutine completes the Runtime closes the loop's event store, which closes its live
// subscriber channels — so an Observe pump terminates (ranges to completion) without
// relying on client-ctx cancellation or process exit, and no fsstore handle leaks.
func TestObserveChannelClosesOnLoopCompletion(t *testing.T) {
	fake := &scriptedCompleter{responses: []model.CompletionResp{{Content: "done"}}}
	rt := newTestRuntime(t, fake)

	h, err := rt.StartLoop(context.Background(), LoopConfig{Task: "hi", ModelID: "cloud/x"})
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	ch, cancel, err := rt.Observe(h.ID())
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	defer cancel()

	if _, err := h.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	// Draining the channel must terminate (range returns) because store.Close closed
	// the subscriber channel on run completion. A hang here means the pump would never
	// stop. A background timer fails the test rather than hanging the suite forever.
	done := make(chan struct{})
	go func() {
		for range ch {
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Observe channel never closed after loop completion (store not closed)")
	}

	// Attach must STILL work after the store is closed (Replay opens its own reader).
	evs, err := rt.Attach(h.ID(), 0)
	if err != nil {
		t.Fatalf("Attach after completion: %v", err)
	}
	if !hasKind(evs, event.KindTurnStart) {
		t.Errorf("durable history unreadable after store close: %v", evs)
	}
}

func TestSpawnReturnsHandle(t *testing.T) {
	// The root completer blocks so the loop is still RUNNING (its store open) while we
	// Spawn a child — post-FIX-3 the Runtime closes the store on run completion, so a
	// spawn must land on the live store, not after the loop has finished.
	release := make(chan struct{})
	rt := New(Deps{
		BaseDir:       t.TempDir(),
		MaxConcurrent: 2,
		BuildAgent: func(store event.EventStore, tree *agent.AgentTree, modelID string, reg *tools.Registry) (*agent.Agent, agent.ChildBuilder, string, error) {
			childBuilder := func(ctx context.Context, opts agent.SpawnOpts, node *agent.AgentNode, tree *agent.AgentTree) (string, error) {
				return opts.Task + "-out", nil
			}
			return agent.New(blockingCompleter{release: release}, execAll{reg}, nopRenderer{}, modelID, "", 1, 0), childBuilder, modelID, nil
		},
	})

	h, err := rt.StartLoop(context.Background(), LoopConfig{Task: "hi", ModelID: "cloud/x"})
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}

	sh, err := rt.Spawn(context.Background(), h.ID(), SpawnOpts{Label: "child", Task: "x"})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	done := sh.Wait()
	if done.Result != "x-out" {
		t.Fatalf("spawn result = %q, want x-out", done.Result)
	}
	if sh.NodeID() == "" {
		t.Error("spawn handle NodeID() is empty")
	}

	// spawn.start + spawn.done appear on the same event stream (read while the store
	// is still open — the loop is blocked in the completer).
	evs, err := rt.Attach(h.ID(), 0)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if !hasKind(evs, event.KindSpawnStart) || !hasKind(evs, event.KindSpawnDone) {
		t.Errorf("spawn events missing from stream: %v", evs)
	}

	close(release)
	if _, err := h.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}

func TestLoopNotFound(t *testing.T) {
	fake := &scriptedCompleter{}
	rt := newTestRuntime(t, fake)

	if _, _, err := rt.Observe("nope"); !errors.Is(err, ErrLoopNotFound) {
		t.Errorf("Observe err = %v, want ErrLoopNotFound", err)
	}
	if _, err := rt.Attach("nope", 0); !errors.Is(err, ErrLoopNotFound) {
		t.Errorf("Attach err = %v, want ErrLoopNotFound", err)
	}
	if err := rt.Send(context.Background(), "nope", ""); !errors.Is(err, ErrLoopNotFound) {
		t.Errorf("Send err = %v, want ErrLoopNotFound", err)
	}
	if _, err := rt.Spawn(context.Background(), "nope", SpawnOpts{}); !errors.Is(err, ErrLoopNotFound) {
		t.Errorf("Spawn err = %v, want ErrLoopNotFound", err)
	}
}
