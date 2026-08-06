package agent

import (
	"context"
	"errors"
	"testing"
)

// The per-turn strip predicates are unified into Scheduler.Visible /
// Scheduler.StripPredicate (change 0036); their term-by-term coverage lives in
// scheduler_visible_test.go. What remains here are the call-time BACKSTOP tests:
// a spawn that sneaks through while the schema was stripped (a hallucinated call,
// or an in-flight turn that saw the tool) still hits the hard limit. Stripping is
// an optimization; the backstop is the enforcement.

// Backstop: a spawn call that sneaks through while stripped still gets the
// budget-exhausted error. Confirms stripping did not replace the enforcement.
func TestBackstopFiresWhenBudgetExhaustedWhileStripped(t *testing.T) {
	tr := NewAgentTreeWithConcurrency("root", "m", 16)
	tr.SetMaxSpawns(1)
	// Exhaust the budget: one child already created.
	tr.addNode(&AgentNode{ID: newNodeID(), ParentID: tr.RootID(), Depth: 1, Status: StatusDone})

	// Precondition: the unified predicate strips (budget exhausted, permanent).
	if !tr.Scheduler().StripPredicate(tr.RootID())() {
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
