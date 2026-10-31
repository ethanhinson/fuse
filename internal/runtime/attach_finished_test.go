package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/event/fsstore"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/tools"
)

// TestAttachAfterInteractiveLoopEndsOnTheSameReplica guards reattach on the
// replica that ran a loop: once an interactive session ends (idle reap), its
// cached durable sink is bound to the ended session context, so replaying
// through it failed with "context canceled" and a client re-opening Observe
// from seq 0 got an error instead of the history. Attach must read the durable
// store under the caller's context.
func TestAttachAfterInteractiveLoopEndsOnTheSameReplica(t *testing.T) {
	store := fsstore.NewDurableFSStore(t.TempDir())
	fake := newGatedCompleter(model.CompletionResp{Content: "answer"})
	rt := New(Deps{
		DurableStore:  store,
		Registry:      store,
		MaxConcurrent: 1,
		IdleTTL:       120 * time.Millisecond,
		BuildAgent: func(s event.EventStore, tree *agent.AgentTree, modelID string, r *tools.Registry) (*agent.Agent, agent.ChildBuilder, string, error) {
			return agent.New(fake, execAll{r}, nopRenderer{}, modelID, "", 1, 0), nil, modelID, nil
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, err := rt.StartLoop(ctx, LoopConfig{Task: "hello", Interactive: true})
	if err != nil {
		t.Fatal(err)
	}
	ev, unsub, err := rt.Observe(ctx, event.DefaultTenant, h.ID())
	if err != nil {
		t.Fatal(err)
	}
	fake.admit(0)
	waitForKind(t, ev, event.KindLoopParked, 2*time.Second)
	unsub()
	select {
	case <-doneOf(h):
	case <-time.After(2 * time.Second):
		t.Fatal("the loop was not reaped")
	}
	hist, err := rt.Attach(ctx, event.DefaultTenant, h.ID(), 0)
	if err != nil {
		t.Fatalf("Attach after the session ended: %v", err)
	}
	if !hasKind(hist, event.KindLoopParked) {
		t.Fatalf("history missing loop.parked: %v", kindsOf(hist))
	}
}
