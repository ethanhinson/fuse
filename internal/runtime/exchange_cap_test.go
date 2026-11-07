package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/tools"
)

// toolForeverCompleter always answers with a (distinct) tool call and reports usage,
// so only a turn cap or a token ceiling can stop it. It never trips the doom-loop
// detector because every call carries unique arguments.
type toolForeverCompleter struct{ calls atomic.Int32 }

func (c *toolForeverCompleter) Complete(ctx context.Context, req model.CompletionReq) (model.CompletionResp, error) {
	n := c.calls.Add(1)
	return model.CompletionResp{
		ToolCalls:    []model.ToolCall{{ID: fmt.Sprintf("c%d", n), Name: "no_such_tool", Arguments: fmt.Sprintf(`{"n":%d}`, n)}},
		InputTokens:  600,
		OutputTokens: 400,
	}, nil
}

// A loop keeps ErrMaxTurns.
func TestMaxTurnsStillErrors(t *testing.T) {
	fake := &toolForeverCompleter{}
	rt := New(Deps{
		BaseDir:       t.TempDir(),
		MaxConcurrent: 1,
		BuildAgent: func(store event.EventStore, tr *agent.AgentTree, modelID string, reg *tools.Registry) (*agent.Agent, error) {
			a := agent.New(fake, execAll{reg}, nopRenderer{}, modelID, "", 2, 0)
			a.SetMaxTurnsExplicit(true)
			return a, nil
		},
	})
	h, err := rt.StartLoop(context.Background(), LoopConfig{Task: "go", ModelID: "cloud/x"})
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	if _, err := h.Wait(); err == nil || err.Error() != agent.ErrMaxTurns.Error() {
		t.Fatalf("Wait err = %v, want ErrMaxTurns", err)
	}
	if n := fake.calls.Load(); n != 2 {
		t.Fatalf("calls = %d, want 2", n)
	}
}

func tokenCeilingRuntime(t *testing.T, fake *toolForeverCompleter, maxTurns int, ceiling int, treeOut **agent.AgentTree) Runtime {
	t.Helper()
	return New(Deps{
		BaseDir:       t.TempDir(),
		MaxConcurrent: 1,
		BuildAgent: func(store event.EventStore, tr *agent.AgentTree, modelID string, reg *tools.Registry) (*agent.Agent, error) {
			*treeOut = tr
			tr.Scheduler().SetSessionTokens(ceiling)
			a := agent.New(fake, execAll{reg}, nopRenderer{}, modelID, "", maxTurns, 0)
			return a, nil
		},
	})
}

// A non-spawning loop stops calling the model once the session token
// ceiling is crossed and ends with ErrTokenQuotaExhausted.
func TestSessionTokenCeilingStopsLoop(t *testing.T) {
	fake := &toolForeverCompleter{}
	var tree *agent.AgentTree
	rt := tokenCeilingRuntime(t, fake, 0, 1500, &tree)
	h, err := rt.StartLoop(context.Background(), LoopConfig{Task: "go", ModelID: "cloud/x"})
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	if _, err := h.Wait(); !errors.Is(err, agent.ErrTokenQuotaExhausted) {
		t.Fatalf("Wait err = %v, want ErrTokenQuotaExhausted", err)
	}
	// Call 1 spends 1000 (< 1500), call 2 spends 2000 (>= 1500), call 3 is refused.
	if n := fake.calls.Load(); n != 2 {
		t.Fatalf("model calls = %d, want 2", n)
	}
	if got := tree.SessionTokens(); got != 2000 {
		t.Fatalf("SessionTokens = %d, want 2000", got)
	}
	if tree.Scheduler().Visible(tree.RootID()) {
		t.Fatal("spawn_agent still visible after the ceiling was crossed")
	}
}
