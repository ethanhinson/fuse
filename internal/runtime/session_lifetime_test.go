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

// TestInteractiveLoopSurvivesRequestCtxCancel is D6 test 2: an interactive loop runs
// under a SESSION-scoped context distinct from the request/connection ctx, so a
// client disconnect (request ctx cancel) does NOT cancel the parked run. After the
// disconnect the loop is still live — a subsequent Send drives another turn instead
// of returning ErrLoopFinished.
func TestInteractiveLoopSurvivesRequestCtxCancel(t *testing.T) {
	fake := newGatedCompleter(
		model.CompletionResp{Content: "first answer"},  // turn 0 -> park
		model.CompletionResp{Content: "second answer"}, // turn 1 after post-disconnect Send -> park
	)
	// A long idle TTL so the reaper never fires during this test — we are proving the
	// disconnect alone does not end the loop.
	rt := New(Deps{
		BaseDir:       t.TempDir(),
		MaxConcurrent: 1,
		IdleTTL:       time.Hour,
		BuildAgent: func(store event.EventStore, tree *agent.AgentTree, modelID string, r *tools.Registry) (*agent.Agent, agent.ChildBuilder, string, error) {
			return agent.New(fake, execAll{r}, nopRenderer{}, modelID, "", 1, 0), nil, modelID, nil
		},
	})

	// The REQUEST context — this models one client connection. Interactive loops must
	// outlive its cancellation.
	reqCtx, disconnect := context.WithCancel(context.Background())

	h, err := rt.StartLoop(reqCtx, LoopConfig{Task: "hi", ModelID: "cloud/x", Interactive: true})
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	loopID := h.ID()

	// Observe on a SEPARATE long-lived context (a client would re-open Observe on a
	// fresh connection); subscribe before the first turn so we don't miss the park.
	obsCtx, obsCancel := context.WithCancel(context.Background())
	defer obsCancel()
	evCh, unsub, err := rt.Observe(obsCtx, event.DefaultTenant, loopID)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	defer unsub()

	fake.admit(0)
	waitForKind(t, evCh, event.KindLoopParked, 2*time.Second)

	// Client disconnects: cancel the request ctx. A connection-pinned loop would
	// unwind here and flip to finished.
	disconnect()
	// Give any (incorrect) cancellation a moment to propagate and finish the run.
	time.Sleep(50 * time.Millisecond)

	// The loop must still be live: Send drives another turn, not ErrLoopFinished.
	fake.admit(1)
	if err := rt.Send(context.Background(), event.DefaultTenant, loopID, "still there?"); err != nil {
		t.Fatalf("Send after request-ctx cancel should succeed (session survives disconnect), got: %v", err)
	}
	waitForKind(t, evCh, event.KindLoopParked, 2*time.Second)
}

// TestInteractiveLoopIdleReap is D6 test 4: with a short idle TTL, a session that
// receives no Send and has no live Observe within the window is reaped — the
// registry is flipped not-live and the run goroutine exits (its store handle
// released) without leaking. Reap is the session's bound (D2): survive disconnect,
// but not forever.
func TestInteractiveLoopIdleReap(t *testing.T) {
	fake := newGatedCompleter(
		model.CompletionResp{Content: "only answer"}, // turn 0 -> park, then idle-reaped
	)
	reg := newFakeRegistry()
	rt := New(Deps{
		BaseDir:       t.TempDir(),
		MaxConcurrent: 1,
		Registry:      reg,
		DurableStore:  nil, // registry-only liveness tracking is enough for this assertion
		IdleTTL:       80 * time.Millisecond,
		LeaseTTL:      time.Hour, // keep the lease renewer out of the way
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
	// Drop the live Observe so the loop is genuinely idle (no Send, no live observer).
	unsub()

	// Within a few idle-TTL windows the reaper cancels the session ctx and the run
	// goroutine exits. Wait bounded on the handle.
	select {
	case <-doneOf(h):
	case <-time.After(2 * time.Second):
		t.Fatal("idle interactive loop was not reaped within the timeout")
	}

	// The registry must record the loop as not-live after reap.
	rec, ok := reg.record(event.StreamKey{Tenant: event.DefaultTenant, Loop: event.LoopID(h.ID())})
	if !ok {
		t.Fatal("loop record missing from registry after reap")
	}
	if rec.Live {
		t.Fatal("reaped loop is still marked Live in the registry")
	}
}
