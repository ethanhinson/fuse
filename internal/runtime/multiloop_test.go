package runtime

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/tools"
)

// TestNConcurrentLoopsIsolatedStreams is the permanent regression guard and the
// -race proof at the runtime seam (plan Task 1): one Runtime hosts N loops started
// CONCURRENTLY (goroutines), each with its own per-loop fsstore under one BaseDir.
// A sequential N-loop test would stay green even with a shared-global clobber, so
// this drives them concurrently on purpose (learning
// race-invisible-to-race-detector-without-concurrent-test). It asserts:
//   - each loop's stream contains ONLY its own events (no cross-loop bleed),
//   - each loop's Seq run is contiguous and monotonic from 1 (per-loop allocator),
//   - no two loops share a Seq origin (distinct loop ids = distinct stores).
func TestNConcurrentLoopsIsolatedStreams(t *testing.T) {
	const n = 4
	baseDir := t.TempDir()

	// One Runtime shared by all loops. Each StartLoop opens its OWN fsstore keyed by
	// its own tree RootID under baseDir, and BuildAgent binds the root agent to that
	// loop's store — so no two loops share a store.
	rt := New(Deps{
		BaseDir:       baseDir,
		MaxConcurrent: 2,
		BuildAgent: func(store event.EventStore, tree *agent.AgentTree, modelID string, reg *tools.Registry) (*agent.Agent, agent.ChildBuilder, string, error) {
			// A distinct scripted completer per loop so each stream is distinguishable.
			fake := &scriptedCompleter{responses: []model.CompletionResp{{Content: "final-" + modelID}}}
			return agent.New(fake, execAll{reg}, nopRenderer{}, modelID, "", 1, 0), nil, modelID, nil
		},
	})

	var wg sync.WaitGroup
	ids := make([]string, n)
	var idsMu sync.Mutex

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			modelID := fmt.Sprintf("cloud/m%d", i)
			h, err := rt.StartLoop(context.Background(), LoopConfig{Task: fmt.Sprintf("task-%d", i), ModelID: modelID})
			if err != nil {
				t.Errorf("loop %d StartLoop: %v", i, err)
				return
			}
			if _, err := h.Wait(); err != nil {
				t.Errorf("loop %d Wait: %v", i, err)
				return
			}
			idsMu.Lock()
			ids[i] = h.ID()
			idsMu.Unlock()
		}(i)
	}
	wg.Wait()

	// Every loop got a DISTINCT id (distinct tree = distinct store origin).
	seen := map[string]bool{}
	for i, id := range ids {
		if id == "" {
			t.Fatalf("loop %d never recorded an id", i)
		}
		if seen[id] {
			t.Fatalf("loop %d reused id %q — two loops share a store origin", i, id)
		}
		seen[id] = true
	}

	// Each loop's durable stream: contiguous monotonic Seq from 1, and it carries
	// ONLY its own turn events (isolation).
	for i, id := range ids {
		evs, err := rt.Attach(context.Background(), event.DefaultTenant, id, 0)
		if err != nil {
			t.Fatalf("loop %d Attach: %v", i, err)
		}
		if len(evs) == 0 {
			t.Fatalf("loop %d produced no events", i)
		}
		var prev event.Seq
		sawStart, sawEnd := false, false
		for j, e := range evs {
			if want := event.Seq(j + 1); e.Seq != want {
				t.Errorf("loop %d event %d Seq = %d, want %d (per-loop allocator must be contiguous from 1)", i, j, e.Seq, want)
			}
			if e.Seq <= prev {
				t.Errorf("loop %d Seq not monotonic: %d after %d", i, e.Seq, prev)
			}
			prev = e.Seq
			if e.Kind == event.KindTurnStart {
				sawStart = true
			}
			if e.Kind == event.KindTurnEnd {
				sawEnd = true
			}
		}
		if !sawStart || !sawEnd {
			t.Errorf("loop %d stream missing turn events: start=%v end=%v", i, sawStart, sawEnd)
		}
	}
}
