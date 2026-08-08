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

// TestSpawnTokenQuotaBackstop is the call-time race safety net behind the
// per-turn strip (change 0036): a spawn committed within a turn after a hard
// token quota is exhausted is refused with the ErrTokenQuotaExhausted identity.
func TestSpawnTokenQuotaBackstop(t *testing.T) {
	t.Run("workflow_pool_tokens_refuses_at_quota", func(t *testing.T) {
		tree := NewAgentTree("root", "m")
		rootID := tree.RootID()
		wf := &AgentNode{ID: newNodeID(), ParentID: rootID, Label: "wf", Depth: 1, Status: StatusRunning, WorkflowRoot: "research"}
		tree.addNode(wf)
		// SubtreeTokens (like SubtreeSpawnCount) excludes the pool-root holder, so
		// the quota measures the subtree's CHILDREN's spend. Charge a child.
		child := &AgentNode{ID: newNodeID(), ParentID: wf.ID, Label: "worker", Depth: 2, Status: StatusRunning}
		tree.addNode(child)
		tree.Scheduler().RegisterPool(wf.ID, WorkflowPool{Tokens: 500})
		child.UpdateTokens(400, 200) // subtree spend 600 >= 500

		s := NewSpawner(WithTree(tree), WithNode(child), WithSpawnDepth(child.Depth))
		_, err := s.Spawn(context.Background(), SpawnOpts{Label: "over"})
		if !errors.Is(err, ErrTokenQuotaExhausted) {
			t.Fatalf("expected ErrTokenQuotaExhausted at workflow token quota, got %v", err)
		}
	})

	t.Run("session_ceiling_refuses_at_quota", func(t *testing.T) {
		tree := NewAgentTree("root", "m")
		tree.Scheduler().SetSessionTokens(500)
		tree.Node(tree.RootID()).UpdateTokens(400, 200) // 600 whole-tree >= 500

		s := NewSpawner(WithTree(tree), WithNode(tree.Node(tree.RootID())), WithSpawnDepth(0))
		_, err := s.Spawn(context.Background(), SpawnOpts{Label: "over"})
		if !errors.Is(err, ErrTokenQuotaExhausted) {
			t.Fatalf("expected ErrTokenQuotaExhausted at session ceiling, got %v", err)
		}
	})

	t.Run("below_quota_allows", func(t *testing.T) {
		tree := NewAgentTree("root", "m")
		rootID := tree.RootID()
		wf := &AgentNode{ID: newNodeID(), ParentID: rootID, Label: "wf", Depth: 1, Status: StatusRunning, WorkflowRoot: "research"}
		tree.addNode(wf)
		tree.Scheduler().RegisterPool(wf.ID, WorkflowPool{Tokens: 500})
		tree.Scheduler().SetSessionTokens(10_000)
		wf.UpdateTokens(100, 100) // 200 < 500

		s := NewSpawner(WithTree(tree), WithNode(wf), WithSpawnDepth(wf.Depth))
		h, err := s.Spawn(context.Background(), SpawnOpts{Label: "ok"})
		if err != nil {
			t.Fatalf("spawn below token quota should succeed, got %v", err)
		}
		<-h.Done
	})
}

// TestSpawnWidthCap verifies the tree-global semaphore bounds concurrently
// RUNNING children; excess spawns queue as pending rather than all executing.
// TestSpawnStoresCancelBeforeTreeExposure is the regression test for the
// cancel-func write race (SF-3): a child cancelled immediately after Spawn (via
// tree.CancelNode) must actually be cancelled. Before the fix the node was added
// to the tree before SetCancel ran, so a concurrent CancelNode could read a nil
// cancel func and silently no-op. Here the child blocks until its context is
// cancelled; if CancelNode saw a nil func the child would hang and the test
// would time out.
func TestSpawnStoresCancelBeforeTreeExposure(t *testing.T) {
	tree := NewAgentTree("root", "m")
	started := make(chan struct{})

	s := NewSpawner(WithTree(tree), WithChildBuilder(
		func(ctx context.Context, opts SpawnOpts, node *AgentNode, _ *AgentTree) (string, error) {
			close(started)
			<-ctx.Done() // only returns if the cancel func was wired and fired
			return "", ctx.Err()
		}))

	h, err := s.Spawn(context.Background(), SpawnOpts{Label: "c"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	<-started
	tree.CancelNode(h.NodeID)

	select {
	case d := <-h.Done:
		if !errors.Is(d.Err, context.Canceled) {
			t.Fatalf("child err = %v, want context.Canceled", d.Err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("child not cancelled — CancelNode likely read a nil cancel func")
	}
}

// TestSpawnSlotAccountingBalanced is the regression guard for the slot leak
// (SF-2): after many spawn/complete and spawn/cancel cycles the scheduler's
// granted-slot count must return to zero. A leaked slot would leave SlotsInUse
// permanently elevated and shrink the effective concurrency cap. Run under -race
// to also exercise the cancel-before-exposure ordering concurrently.
func TestSpawnSlotAccountingBalanced(t *testing.T) {
	tree := NewAgentTree("root", "m")
	sched := tree.Scheduler()

	s := NewSpawner(WithTree(tree), WithChildBuilder(
		func(ctx context.Context, opts SpawnOpts, node *AgentNode, _ *AgentTree) (string, error) {
			if opts.Label == "cancel" {
				<-ctx.Done()
				return "", ctx.Err()
			}
			return "ok", nil
		}))

	for i := 0; i < 25; i++ {
		// Alternate clean completion and immediate cancellation.
		h, err := s.Spawn(context.Background(), SpawnOpts{Label: "ok"})
		if err != nil {
			t.Fatalf("spawn ok %d: %v", i, err)
		}
		h.Wait()

		hc, err := s.Spawn(context.Background(), SpawnOpts{Label: "cancel"})
		if err != nil {
			t.Fatalf("spawn cancel %d: %v", i, err)
		}
		tree.CancelNode(hc.NodeID)
		hc.Wait()
	}

	// Slots should have all been released. Poll briefly since release happens on
	// the worker goroutine as it unwinds.
	deadline := time.After(2 * time.Second)
	for {
		if snap := sched.Snapshot(); snap.SlotsInUse == 0 {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("SlotsInUse = %d after balanced cycles, want 0 (slot leak)", sched.Snapshot().SlotsInUse)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

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
