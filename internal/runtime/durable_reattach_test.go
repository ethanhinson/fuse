package runtime

import (
	"context"
	"testing"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/event/fsstore"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/tools"
)

// TestColdRuntimeAttachesToPriorProcessLoop is the load-bearing 0047 test: a FRESH
// runtime B over the SAME durable store + registry, with an EMPTY in-memory loops
// map, resolves and Attaches a loop that runtime A started and finished. Before this
// change a cold instance returned `runtime: loop not found`; now the durable
// registry lets B discover the loop's existence and replay its history.
func TestColdRuntimeAttachesToPriorProcessLoop(t *testing.T) {
	dir := t.TempDir()
	store := fsstore.NewDurableFSStore(dir)
	// The durable fsstore IS its own registry (it satisfies both interfaces).
	var reg event.LoopRegistry = store

	buildAgent := func(s event.EventStore, tree *agent.AgentTree, modelID string, r *tools.Registry) (*agent.Agent, agent.ChildBuilder, string, error) {
		fake := &scriptedCompleter{responses: []model.CompletionResp{{Content: "done"}}}
		return agent.New(fake, execAll{r}, nopRenderer{}, modelID, "", 1, 0), nil, modelID, nil
	}

	// Runtime A: start a loop, drive it to completion (it emits durable events).
	rtA := New(Deps{
		DurableStore:  store,
		Registry:      reg,
		MaxConcurrent: 1,
		BuildAgent:    buildAgent,
	})
	h, err := rtA.StartLoop(context.Background(), LoopConfig{Task: "hi", ModelID: "cloud/x"})
	if err != nil {
		t.Fatalf("A StartLoop: %v", err)
	}
	if _, err := h.Wait(); err != nil {
		t.Fatalf("A Wait: %v", err)
	}
	loopID := h.ID()

	// Runtime B: a FRESH instance with an empty loops map, same durable store+registry.
	rtB := New(Deps{
		DurableStore:  store,
		Registry:      reg,
		MaxConcurrent: 1,
		BuildAgent:    buildAgent,
	})
	// The map miss on B must fall through to the durable registry (Resolve) and replay.
	hist, err := rtB.Attach(context.Background(), event.DefaultTenant, loopID, 0)
	if err != nil {
		t.Fatalf("cold attach failed (the 0047 bug): %v", err)
	}
	if len(hist) == 0 {
		t.Fatalf("cold attach returned no history")
	}
	if !hasKind(hist, event.KindTurnStart) || !hasKind(hist, event.KindTurnEnd) {
		t.Fatalf("cold attach history missing turn events: %v", hist)
	}

	// A resolve for an unknown loop on B maps to ErrLoopNotFound (not ErrLoopUnknown).
	if _, err := rtB.Attach(context.Background(), event.DefaultTenant, "never-existed", 0); err == nil {
		t.Fatal("Attach of unknown loop should error")
	}
}
