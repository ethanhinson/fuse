package runtime

import (
	"context"
	"errors"
	"testing"

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

func newTestRuntime(t *testing.T, fake agent.Completer) *inProcRuntime {
	t.Helper()
	return New(Deps{
		BaseDir:       t.TempDir(),
		MaxConcurrent: 2,
		BuildAgent: func(modelID string, reg *tools.Registry) (*agent.Agent, string, error) {
			return agent.New(fake, execAll{reg}, nopRenderer{}, modelID, "", 1, 0), modelID, nil
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

func TestSendEnqueuesForRoot(t *testing.T) {
	fake := &scriptedCompleter{responses: []model.CompletionResp{{Content: "done"}}}
	rt := newTestRuntime(t, fake)

	h, err := rt.StartLoop(context.Background(), LoopConfig{Task: "hi", ModelID: "cloud/x"})
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	if _, err := h.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	if err := rt.Send(context.Background(), h.ID(), "more work"); err != nil {
		t.Fatalf("Send: %v", err)
	}

	// The message is enqueued on the root node's human-bus queue (ADR-0022).
	lp := rt.loops[h.ID()]
	pending := lp.humanBus.Pending(h.ID())
	if len(pending) != 1 || pending[0].Text != "more work" {
		t.Fatalf("root queue = %+v, want one 'more work' message", pending)
	}
}

func TestSpawnReturnsHandle(t *testing.T) {
	fake := &scriptedCompleter{responses: []model.CompletionResp{{Content: "done"}}}
	rt := New(Deps{
		BaseDir:       t.TempDir(),
		MaxConcurrent: 2,
		BuildAgent: func(modelID string, reg *tools.Registry) (*agent.Agent, string, error) {
			return agent.New(fake, execAll{reg}, nopRenderer{}, modelID, "", 1, 0), modelID, nil
		},
		BuildChild: func(ctx context.Context, opts agent.SpawnOpts, node *agent.AgentNode, tree *agent.AgentTree) (string, error) {
			return opts.Task + "-out", nil
		},
	})

	h, err := rt.StartLoop(context.Background(), LoopConfig{Task: "hi", ModelID: "cloud/x"})
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	if _, err := h.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
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

	// spawn.start + spawn.done appear on the same event stream.
	evs, err := rt.Attach(h.ID(), 0)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if !hasKind(evs, event.KindSpawnStart) || !hasKind(evs, event.KindSpawnDone) {
		t.Errorf("spawn events missing from stream: %v", evs)
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
