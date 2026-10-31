package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

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

func parkedReason(t *testing.T, ev event.Event) string {
	t.Helper()
	var p event.LoopParkedPayload
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		t.Fatalf("decode loop.parked: %v", err)
	}
	return p.Reason
}

// nthParked returns the n-th (1-based) loop.parked event from the loop's durable
// history. It reads history rather than a live Observe tail: the tail does not
// replay events published before it subscribed, and a fast loop can park before
// the test's subscription exists.
func nthParked(t *testing.T, rt Runtime, loopID string, n int) event.Event {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		hist, err := rt.Attach(context.Background(), event.DefaultTenant, loopID, 0)
		if err != nil {
			t.Fatalf("Attach: %v", err)
		}
		seen := 0
		for _, ev := range hist {
			if ev.Kind == event.KindLoopParked {
				seen++
				if seen == n {
					return ev
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for loop.parked #%d (saw %d)", n, seen)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// An explicit max_turns caps turns PER EXCHANGE in an interactive loop: on the cap the
// loop parks with reason max_turns (the session survives) and the next Send gets a
// fresh allowance (change 0090).
func TestInteractiveMaxTurnsIsPerExchange(t *testing.T) {
	fake := &toolForeverCompleter{}
	var tree *agent.AgentTree
	rt := New(Deps{
		BaseDir:       t.TempDir(),
		MaxConcurrent: 1,
		BuildAgent: func(store event.EventStore, tr *agent.AgentTree, modelID string, reg *tools.Registry) (*agent.Agent, agent.ChildBuilder, string, error) {
			tree = tr
			a := agent.New(fake, execAll{reg}, nopRenderer{}, modelID, "", 2, 0)
			a.SetMaxTurnsExplicit(true)
			return a, nil, modelID, nil
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, err := rt.StartLoop(ctx, LoopConfig{Task: "go", ModelID: "cloud/x", Interactive: true})
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}

	ev := nthParked(t, rt, h.ID(), 1)
	if got := parkedReason(t, ev); got != "max_turns" {
		t.Fatalf("reason = %q, want max_turns", got)
	}
	if n := fake.calls.Load(); n != 2 {
		t.Fatalf("model calls in first exchange = %d, want 2", n)
	}

	if err := rt.Send(ctx, event.DefaultTenant, h.ID(), "again"); err != nil {
		t.Fatalf("Send after cap: %v", err)
	}
	ev = nthParked(t, rt, h.ID(), 2)
	if got := parkedReason(t, ev); got != "max_turns" {
		t.Fatalf("second reason = %q, want max_turns", got)
	}
	if n := fake.calls.Load(); n != 4 {
		t.Fatalf("model calls after second exchange = %d, want 4", n)
	}
	// Usage reaches the tree through the agent-owned sink (no TUI renderer here).
	if got := tree.SessionTokens(); got != 4*1000 {
		t.Fatalf("SessionTokens = %d, want 4000", got)
	}
}

// A non-interactive loop keeps ErrMaxTurns.
func TestNonInteractiveMaxTurnsStillErrors(t *testing.T) {
	fake := &toolForeverCompleter{}
	rt := New(Deps{
		BaseDir:       t.TempDir(),
		MaxConcurrent: 1,
		BuildAgent: func(store event.EventStore, tr *agent.AgentTree, modelID string, reg *tools.Registry) (*agent.Agent, agent.ChildBuilder, string, error) {
			a := agent.New(fake, execAll{reg}, nopRenderer{}, modelID, "", 2, 0)
			a.SetMaxTurnsExplicit(true)
			return a, nil, modelID, nil
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
		BuildAgent: func(store event.EventStore, tr *agent.AgentTree, modelID string, reg *tools.Registry) (*agent.Agent, agent.ChildBuilder, string, error) {
			*treeOut = tr
			tr.Scheduler().SetSessionTokens(ceiling)
			a := agent.New(fake, execAll{reg}, nopRenderer{}, modelID, "", maxTurns, 0)
			return a, nil, modelID, nil
		},
	})
}

// A non-spawning non-interactive loop stops calling the model once the session token
// ceiling is crossed and ends with ErrTokenQuotaExhausted.
func TestSessionTokenCeilingStopsNonInteractiveLoop(t *testing.T) {
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

// An interactive loop parks with reason session_tokens, stays attached, and never calls
// the model again, even when further messages arrive.
func TestSessionTokenCeilingParksInteractiveLoop(t *testing.T) {
	fake := &toolForeverCompleter{}
	var tree *agent.AgentTree
	rt := tokenCeilingRuntime(t, fake, 0, 1500, &tree)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, err := rt.StartLoop(ctx, LoopConfig{Task: "go", ModelID: "cloud/x", Interactive: true})
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	if got := parkedReason(t, nthParked(t, rt, h.ID(), 1)); got != "session_tokens" {
		t.Fatalf("reason = %q, want session_tokens", got)
	}
	if n := fake.calls.Load(); n != 2 {
		t.Fatalf("model calls = %d, want 2", n)
	}
	if got := tree.SessionTokens(); got != 2000 {
		t.Fatalf("SessionTokens = %d, want 2000", got)
	}
	if tree.Scheduler().Visible(tree.RootID()) {
		t.Fatal("spawn_agent still visible after the ceiling was crossed")
	}
	if err := rt.Send(ctx, event.DefaultTenant, h.ID(), "again"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := parkedReason(t, nthParked(t, rt, h.ID(), 2)); got != "session_tokens" {
		t.Fatalf("second reason = %q, want session_tokens", got)
	}
	if n := fake.calls.Load(); n != 2 {
		t.Fatalf("model called again while over the ceiling: calls = %d", n)
	}

	// Closing while parked on the token ceiling ends cleanly, like the normal park.
	if err := rt.(*inProcRuntime).Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if _, err := h.Wait(); err != nil {
		t.Fatalf("Wait after close while parked = %v, want nil", err)
	}
}

// Closing a loop parked at the per-exchange turn cap ends cleanly (nil), matching the
// normal park.
func TestCloseWhileParkedAtTurnCapEndsCleanly(t *testing.T) {
	fake := &toolForeverCompleter{}
	rt := New(Deps{
		BaseDir:       t.TempDir(),
		MaxConcurrent: 1,
		BuildAgent: func(store event.EventStore, tr *agent.AgentTree, modelID string, reg *tools.Registry) (*agent.Agent, agent.ChildBuilder, string, error) {
			a := agent.New(fake, execAll{reg}, nopRenderer{}, modelID, "", 2, 0)
			a.SetMaxTurnsExplicit(true)
			return a, nil, modelID, nil
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, err := rt.StartLoop(ctx, LoopConfig{Task: "go", ModelID: "cloud/x", Interactive: true})
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	if got := parkedReason(t, nthParked(t, rt, h.ID(), 1)); got != "max_turns" {
		t.Fatalf("reason = %q, want max_turns", got)
	}
	if err := rt.(*inProcRuntime).Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if _, err := h.Wait(); err != nil {
		t.Fatalf("Wait after close at cap park = %v, want nil", err)
	}
}
