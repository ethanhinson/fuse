package agent

import (
	"context"
	"errors"
	"testing"
)

// The Scheduler is the single admission authority (change 0036). Task 1 is a
// behavior-preserving refactor: the semaphore, the lifetime budget, and the
// yield/unyield refcounting move behind Scheduler methods, but the admission
// VERDICT it computes must mirror the brakes that 0033/0034 already enforce
// (active cap, lifetime budget, depth) so Tasks 2/3 can build the queue and the
// unified visibility predicate on top of it.

func TestSchedulerBudgetDelegates(t *testing.T) {
	tr := NewAgentTree("root", "m")
	sc := tr.Scheduler()
	sc.SetMaxSpawns(3)

	if used, max := sc.SpawnBudget(); used != 0 || max != 3 {
		t.Fatalf("fresh scheduler budget = %d/%d, want 0/3", used, max)
	}
	// The tree's shim must report the same figures — it delegates to the
	// scheduler rather than owning a second counter.
	if used, max := tr.SpawnBudget(); used != 0 || max != 3 {
		t.Fatalf("tree shim budget = %d/%d, want 0/3 (delegated)", used, max)
	}

	tr.addNode(&AgentNode{ID: newNodeID(), ParentID: tr.RootID(), Depth: 1, Status: StatusDone})
	if used, _ := sc.SpawnBudget(); used != 1 {
		t.Fatalf("after one spawn used = %d, want 1", used)
	}
}

// Admit computes the current verdict for a spawn from a given scope. Granted
// below the active cap; Queued at/above it (the semaphore would block, which is
// where Task 2 installs the fair queue); Denied on budget/depth with the
// existing error identities preserved as the backstop.
func TestSchedulerAdmitGrantedBelowCap(t *testing.T) {
	tr := NewAgentTreeWithConcurrency("root", "m", 2)
	sc := tr.Scheduler()

	v, err := sc.Admit(AdmitRequest{Depth: 0})
	if v != Granted || err != nil {
		t.Fatalf("empty tree, cap 2: verdict=%v err=%v, want Granted/nil", v, err)
	}
}

func TestSchedulerAdmitQueuedAtCap(t *testing.T) {
	tr := NewAgentTreeWithConcurrency("root", "m", 2)
	sc := tr.Scheduler()
	// Two active children saturate the global slot cap.
	tr.addNode(&AgentNode{ID: newNodeID(), ParentID: tr.RootID(), Depth: 1, Status: StatusRunning})
	tr.addNode(&AgentNode{ID: newNodeID(), ParentID: tr.RootID(), Depth: 1, Status: StatusRunning})

	v, err := sc.Admit(AdmitRequest{Depth: 0})
	if v != Queued || err != nil {
		t.Fatalf("at cap: verdict=%v err=%v, want Queued/nil", v, err)
	}
}

func TestSchedulerAdmitDeniedBudgetExhausted(t *testing.T) {
	tr := NewAgentTreeWithConcurrency("root", "m", 16)
	sc := tr.Scheduler()
	sc.SetMaxSpawns(1)
	tr.addNode(&AgentNode{ID: newNodeID(), ParentID: tr.RootID(), Depth: 1, Status: StatusDone})

	v, err := sc.Admit(AdmitRequest{Depth: 0})
	if v != Denied {
		t.Fatalf("budget exhausted: verdict=%v, want Denied", v)
	}
	if !errors.Is(err, ErrSpawnBudgetExhausted) {
		t.Fatalf("budget exhausted: err=%v, want ErrSpawnBudgetExhausted identity", err)
	}
}

func TestSchedulerAdmitDeniedDepthExceeded(t *testing.T) {
	tr := NewAgentTree("root", "m")
	sc := tr.Scheduler()

	// A request at Depth==MaxDepth would create a child at MaxDepth+1.
	v, err := sc.Admit(AdmitRequest{Depth: MaxDepth})
	if v != Denied {
		t.Fatalf("depth at limit: verdict=%v, want Denied", v)
	}
	if !errors.Is(err, ErrMaxDepthExceeded) {
		t.Fatalf("depth at limit: err=%v, want ErrMaxDepthExceeded identity", err)
	}
}

// The scheduler owns the global slot cap now (the fair queue replaced the raw
// channel in Task 2); its capacity is the concurrency figure passed at
// construction, falling back to MaxConcurrentSpawns for <= 0.
func TestSchedulerSemaphoreCapacity(t *testing.T) {
	if got := NewAgentTreeWithConcurrency("r", "m", 3).Scheduler().slotCap; got != 3 {
		t.Fatalf("scheduler slot cap = %d, want 3", got)
	}
	if got := NewAgentTreeWithConcurrency("r", "m", 0).Scheduler().slotCap; got != MaxConcurrentSpawns {
		t.Fatalf("scheduler slot cap = %d, want %d (zero fallback)", got, MaxConcurrentSpawns)
	}
}

// Yield/unyield refcounting is the depth-2 freeze fix (learning #12). It moves
// behind the scheduler but keeps its exact semantics: the first concurrent wait
// releases the node's slot, the last re-acquires; root (Depth 0) is exempt.
func TestSchedulerYieldRefcountSemantics(t *testing.T) {
	tr := NewAgentTreeWithConcurrency("root", "m", 1)
	sc := tr.Scheduler()

	// A depth-1 node holds the single slot.
	if err := sc.acquireSlot(context.Background(), ""); err != nil {
		t.Fatal("first acquire should succeed on an empty cap-1 pool")
	}
	node := &AgentNode{ID: newNodeID(), Depth: 1}

	// First yield releases the slot; a second concurrent wait does not.
	sc.YieldSlot(node)
	sc.YieldSlot(node)
	// The slot is free now, so a fresh acquire must succeed without blocking.
	if err := sc.acquireSlot(context.Background(), ""); err != nil {
		t.Fatal("after yield the slot should be free")
	}
	sc.releaseSlot() // give it back so unyield can re-take it

	// Two unyields: only the last (refcount hits zero) re-acquires.
	if !sc.UnyieldSlot(context.Background(), node) {
		t.Fatal("non-final unyield must not block and must return true")
	}
	if !sc.UnyieldSlot(context.Background(), node) {
		t.Fatal("final unyield should re-acquire the free slot")
	}

	// Root is exempt: yield/unyield are no-ops that never touch the semaphore.
	root := &AgentNode{ID: tr.RootID(), Depth: 0}
	sc.YieldSlot(root)
	if !sc.UnyieldSlot(context.Background(), root) {
		t.Fatal("root unyield must be a no-op returning true")
	}
}
