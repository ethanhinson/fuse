package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestSpawnerDepthLimit(t *testing.T) {
	t.Run("at_max_depth_returns_error", func(t *testing.T) {
		s := NewSpawner(WithSpawnDepth(MaxDepth))
		_, err := s.Spawn(context.Background(), SpawnOpts{Label: "child"})
		if !errors.Is(err, ErrMaxDepthExceeded) {
			t.Fatalf("expected ErrMaxDepthExceeded, got %v", err)
		}
	})

	t.Run("below_max_depth_creates_node", func(t *testing.T) {
		tree := NewAgentTree("root", "m")
		s := NewSpawner(WithSpawnDepth(MaxDepth-1), WithTree(tree))
		h, err := s.Spawn(context.Background(), SpawnOpts{Label: "child"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if h.NodeID == "" {
			t.Fatal("NodeID must be non-empty for a successful spawn")
		}
		// drain done channel to avoid goroutine leak
		<-h.Done
	})
}

// TestSpawnBudgetBackstop verifies the tree-global spawn budget refuses a spawn
// once the tree is at its ceiling, with ErrSpawnBudgetExhausted — the backstop
// that makes the injected budget line safe if the model ignores it.
func TestSpawnBudgetBackstop(t *testing.T) {
	t.Run("refuses_at_ceiling", func(t *testing.T) {
		tree := NewAgentTree("root", "m")
		tree.SetMaxSpawns(2)
		// Pre-fill the tree to the ceiling (2 children already created).
		rootID := tree.RootID()
		tree.addNode(&AgentNode{ID: newNodeID(), ParentID: rootID, Label: "c1"})
		tree.addNode(&AgentNode{ID: newNodeID(), ParentID: rootID, Label: "c2"})

		s := NewSpawner(WithTree(tree))
		_, err := s.Spawn(context.Background(), SpawnOpts{Label: "over"})
		if !errors.Is(err, ErrSpawnBudgetExhausted) {
			t.Fatalf("expected ErrSpawnBudgetExhausted at ceiling, got %v", err)
		}
	})

	t.Run("allows_below_ceiling", func(t *testing.T) {
		tree := NewAgentTree("root", "m")
		tree.SetMaxSpawns(4)
		s := NewSpawner(WithTree(tree))
		h, err := s.Spawn(context.Background(), SpawnOpts{Label: "ok"})
		if err != nil {
			t.Fatalf("spawn below ceiling should succeed, got %v", err)
		}
		<-h.Done
	})

	t.Run("unset_budget_never_refuses", func(t *testing.T) {
		tree := NewAgentTree("root", "m") // no SetMaxSpawns
		rootID := tree.RootID()
		for i := 0; i < 50; i++ {
			tree.addNode(&AgentNode{ID: newNodeID(), ParentID: rootID})
		}
		s := NewSpawner(WithTree(tree))
		h, err := s.Spawn(context.Background(), SpawnOpts{Label: "ok"})
		if err != nil {
			t.Fatalf("unset budget must not refuse, got %v", err)
		}
		<-h.Done
	})
}

// TestSpawnWidthCap verifies the tree-global semaphore bounds concurrently
// RUNNING children; excess spawns queue as pending rather than all executing.
func TestSpawnWidthCap(t *testing.T) {
	tree := NewAgentTree("root", "m")
	var running, peak atomic.Int32
	release := make(chan struct{})

	s := NewSpawner(WithTree(tree), WithChildBuilder(
		func(ctx context.Context, opts SpawnOpts, node *AgentNode, _ *AgentTree) (string, error) {
			cur := running.Add(1)
			for {
				p := peak.Load()
				if cur <= p || peak.CompareAndSwap(p, cur) {
					break
				}
			}
			<-release
			running.Add(-1)
			return "ok", nil
		}))

	total := MaxConcurrentSpawns + 4
	handles := make([]AgentHandle, 0, total)
	for i := 0; i < total; i++ {
		h, err := s.Spawn(context.Background(), SpawnOpts{Label: "w"})
		if err != nil {
			t.Fatalf("spawn %d: %v", i, err)
		}
		handles = append(handles, h)
	}

	// Give the runnable children time to start and the rest time to queue.
	deadline := time.After(2 * time.Second)
	for running.Load() < int32(MaxConcurrentSpawns) {
		select {
		case <-deadline:
			t.Fatalf("only %d children started; want %d", running.Load(), MaxConcurrentSpawns)
		case <-time.After(5 * time.Millisecond):
		}
	}

	close(release)
	for _, h := range handles {
		if d := h.Wait(); d.Err != nil {
			t.Fatalf("child error: %v", d.Err)
		}
	}
	if p := peak.Load(); p > int32(MaxConcurrentSpawns) {
		t.Errorf("peak concurrency %d exceeded cap %d", p, MaxConcurrentSpawns)
	}
}

// TestNestedSpawnsDoNotDeadlockOnWidthCap reproduces the live freeze: more
// parent agents than slots, each spawning a child and blocking on it. Parents
// must yield their slot while waiting (YieldSlot/UnyieldSlot around Wait) or
// every slot is held by a blocked parent and no child can ever start.
func TestNestedSpawnsDoNotDeadlockOnWidthCap(t *testing.T) {
	tree := NewAgentTree("root", "m")
	rootNode := tree.Node(tree.RootID())

	// Leaf children complete immediately.
	leafBuilder := func(ctx context.Context, opts SpawnOpts, node *AgentNode, _ *AgentTree) (string, error) {
		return "leaf-done", nil
	}
	// Parent children each spawn one leaf and block on it, yielding their
	// slot around the wait exactly like cmd/fuse's SpawnFunc wrapper.
	parentBuilder := func(ctx context.Context, opts SpawnOpts, node *AgentNode, _ *AgentTree) (string, error) {
		leafSpawner := NewSpawner(WithTree(tree), WithNode(node), WithSpawnDepth(node.Depth), WithChildBuilder(leafBuilder))
		h, err := leafSpawner.Spawn(ctx, SpawnOpts{Label: "leaf"})
		if err != nil {
			return "", err
		}
		tree.YieldSlot(node)
		done := h.Wait()
		if !tree.UnyieldSlot(ctx, node) {
			return "", ctx.Err()
		}
		return done.Result, done.Err
	}

	s := NewSpawner(WithTree(tree), WithNode(rootNode), WithSpawnDepth(0), WithChildBuilder(parentBuilder))
	total := MaxConcurrentSpawns + 4 // more parents than slots
	handles := make([]AgentHandle, 0, total)
	for i := 0; i < total; i++ {
		h, err := s.Spawn(context.Background(), SpawnOpts{Label: "parent"})
		if err != nil {
			t.Fatalf("spawn %d: %v", i, err)
		}
		handles = append(handles, h)
	}

	doneCh := make(chan struct{})
	go func() {
		for _, h := range handles {
			if d := h.Wait(); d.Err != nil || d.Result != "leaf-done" {
				t.Errorf("parent result = %+v", d)
			}
		}
		close(doneCh)
	}()
	select {
	case <-doneCh:
	case <-time.After(10 * time.Second):
		t.Fatal("deadlock: nested spawns starved the width cap")
	}
}
