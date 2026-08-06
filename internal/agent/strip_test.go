package agent

import (
	"context"
	"errors"
	"testing"
)

func TestStripPredicateNilTreeNeverStrips(t *testing.T) {
	p := NewStripSpawnPredicate(nil, 16)
	if p() {
		t.Fatal("nil-tree predicate should never strip")
	}
}

func TestStripPredicateBudgetExhausted(t *testing.T) {
	tr := NewAgentTreeWithConcurrency("root", "m", 16)
	tr.SetMaxSpawns(2)
	p := NewStripSpawnPredicate(tr, 16)
	if p() {
		t.Fatal("with 0/2 used, should not strip")
	}
	// Add two child nodes to reach used == max (root excluded from used).
	tr.addNode(&AgentNode{ID: newNodeID(), ParentID: tr.RootID(), Depth: 1, Status: StatusDone})
	tr.addNode(&AgentNode{ID: newNodeID(), ParentID: tr.RootID(), Depth: 1, Status: StatusDone})
	if !p() {
		used, max := tr.SpawnBudget()
		t.Fatalf("with %d/%d used, should strip (permanent)", used, max)
	}
}

func TestStripPredicateBudgetZeroNeverStrips(t *testing.T) {
	tr := NewAgentTreeWithConcurrency("root", "m", 16)
	// max_spawns 0 => no budget strip regardless of node count.
	tr.addNode(&AgentNode{ID: newNodeID(), ParentID: tr.RootID(), Depth: 1, Status: StatusDone})
	p := NewStripSpawnPredicate(tr, 100) // high cap so only budget could trigger
	if p() {
		t.Fatal("max_spawns 0 must never strip via budget")
	}
}

func TestStripPredicateActiveCapReached(t *testing.T) {
	tr := NewAgentTreeWithConcurrency("root", "m", 2)
	p := NewStripSpawnPredicate(tr, 2)
	if p() {
		t.Fatal("no active children, cap 2: should not strip")
	}
	// Two running children => running+pending == cap.
	tr.addNode(&AgentNode{ID: newNodeID(), ParentID: tr.RootID(), Depth: 1, Status: StatusRunning})
	tr.addNode(&AgentNode{ID: newNodeID(), ParentID: tr.RootID(), Depth: 1, Status: StatusRunning})
	if !p() {
		t.Fatal("running+pending >= cap: should strip (reversible)")
	}
}

func TestStripPredicatePendingCountsTowardCap(t *testing.T) {
	tr := NewAgentTreeWithConcurrency("root", "m", 2)
	p := NewStripSpawnPredicate(tr, 2)
	tr.addNode(&AgentNode{ID: newNodeID(), ParentID: tr.RootID(), Depth: 1, Status: StatusRunning})
	tr.addNode(&AgentNode{ID: newNodeID(), ParentID: tr.RootID(), Depth: 1, Status: StatusPending})
	if !p() {
		t.Fatal("1 running + 1 pending == cap 2: should strip")
	}
}

// Reversible cap: strip engages at the cap, then releases when a child finishes.
func TestStripReversibleAtActiveCap(t *testing.T) {
	tr := NewAgentTreeWithConcurrency("root", "m", 2)
	p := NewStripSpawnPredicate(tr, 2)

	c1 := &AgentNode{ID: newNodeID(), ParentID: tr.RootID(), Depth: 1, Status: StatusRunning}
	c2 := &AgentNode{ID: newNodeID(), ParentID: tr.RootID(), Depth: 1, Status: StatusRunning}
	tr.addNode(c1)
	tr.addNode(c2)
	if !p() {
		t.Fatal("at cap: expected strip")
	}
	// A child finishes -> active count drops below cap -> tool returns.
	c1.Finish(StatusDone, "")
	if p() {
		t.Fatal("below cap after finish: expected NO strip (reversible)")
	}
}

// Permanent budget: once used >= max, strip stays engaged even with no active
// children.
func TestStripPermanentAtBudgetExhaustion(t *testing.T) {
	tr := NewAgentTreeWithConcurrency("root", "m", 16)
	tr.SetMaxSpawns(2)
	p := NewStripSpawnPredicate(tr, 16)

	// Two finished (not active) children exhaust the append-only budget.
	tr.addNode(&AgentNode{ID: newNodeID(), ParentID: tr.RootID(), Depth: 1, Status: StatusDone})
	tr.addNode(&AgentNode{ID: newNodeID(), ParentID: tr.RootID(), Depth: 1, Status: StatusDone})
	if r, pend := tr.ActiveCounts(); r != 0 || pend != 0 {
		t.Fatalf("expected zero active, got running=%d pending=%d", r, pend)
	}
	if !p() {
		t.Fatal("budget exhausted with zero active: expected permanent strip")
	}
}

// Backstop: a spawn call that sneaks through while stripped (e.g. a
// hallucinated call, or an in-flight turn that saw the schema) still gets the
// budget-exhausted error. Confirms stripping did not replace the enforcement.
func TestBackstopFiresWhenBudgetExhaustedWhileStripped(t *testing.T) {
	tr := NewAgentTreeWithConcurrency("root", "m", 16)
	tr.SetMaxSpawns(1)
	// Exhaust the budget: one child already created.
	tr.addNode(&AgentNode{ID: newNodeID(), ParentID: tr.RootID(), Depth: 1, Status: StatusDone})

	p := NewStripSpawnPredicate(tr, 16)
	if !p() {
		t.Fatal("precondition: budget should be exhausted -> stripped")
	}

	root := tr.Node(tr.RootID())
	s := NewSpawner(WithTree(tr), WithNode(root), WithSpawnDepth(0))
	_, err := s.Spawn(context.Background(), SpawnOpts{Label: "sneaky", Task: "do"})
	if !errors.Is(err, ErrSpawnBudgetExhausted) {
		t.Fatalf("expected ErrSpawnBudgetExhausted backstop, got %v", err)
	}
}

// Backstop: a spawn at MaxDepth still errors even though depth stripping is the
// primary mechanism (static registry omission happens in cmd/fuse, not here).
func TestBackstopFiresAtMaxDepth(t *testing.T) {
	tr := NewAgentTreeWithConcurrency("root", "m", 16)
	root := tr.Node(tr.RootID())
	// A spawner at depth MaxDepth would create a child at MaxDepth+1.
	s := NewSpawner(WithTree(tr), WithNode(root), WithSpawnDepth(MaxDepth))
	_, err := s.Spawn(context.Background(), SpawnOpts{Label: "deep", Task: "do"})
	if !errors.Is(err, ErrMaxDepthExceeded) {
		t.Fatalf("expected ErrMaxDepthExceeded backstop, got %v", err)
	}
}
