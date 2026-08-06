package agent

import "testing"

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
