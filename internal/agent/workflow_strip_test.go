package agent

import "testing"

// helper: a workflow root subtree with `n` running children directly under it.
func wfTreeWithChildren(t *testing.T, n int) (tree *AgentTree, wfroot string, rootDepth int) {
	t.Helper()
	tree = NewAgentTree("root", "m")
	wf := &AgentNode{ID: newNodeID(), ParentID: tree.RootID(), Label: "wf", Depth: 1, Status: StatusRunning, WorkflowRoot: "research"}
	tree.addNode(wf)
	for i := 0; i < n; i++ {
		tree.addNode(&AgentNode{ID: newNodeID(), ParentID: wf.ID, Depth: 2, Status: StatusRunning})
	}
	return tree, wf.ID, 1
}

func TestWorkflowStripConcurrentReversible(t *testing.T) {
	tree, wfroot, rootDepth := wfTreeWithChildren(t, 0)
	pool := WorkflowPool{Concurrent: 2}
	// The wfroot itself (depth 1) evaluates against its subtree.
	p := NewWorkflowStripPredicate(tree, wfroot, pool, 1, rootDepth)
	if p() {
		t.Fatal("0 active < concurrent 2: should not strip")
	}
	c1 := &AgentNode{ID: newNodeID(), ParentID: wfroot, Depth: 2, Status: StatusRunning}
	c2 := &AgentNode{ID: newNodeID(), ParentID: wfroot, Depth: 2, Status: StatusRunning}
	tree.addNode(c1)
	tree.addNode(c2)
	if !p() {
		t.Fatal("2 active == concurrent 2: should strip (reversible)")
	}
	c1.Finish(StatusDone, "")
	if p() {
		t.Fatal("1 active < concurrent 2 after finish: should NOT strip (reversible)")
	}
}

func TestWorkflowStripTotalPermanent(t *testing.T) {
	tree, wfroot, rootDepth := wfTreeWithChildren(t, 0)
	pool := WorkflowPool{Total: 2}
	p := NewWorkflowStripPredicate(tree, wfroot, pool, 1, rootDepth)
	if p() {
		t.Fatal("0 spawned < total 2: should not strip")
	}
	// Two finished children exhaust the lifetime quota; strip stays even at 0 active.
	tree.addNode(&AgentNode{ID: newNodeID(), ParentID: wfroot, Depth: 2, Status: StatusDone})
	tree.addNode(&AgentNode{ID: newNodeID(), ParentID: wfroot, Depth: 2, Status: StatusDone})
	if !p() {
		t.Fatal("2 spawned == total 2 with 0 active: should strip (permanent)")
	}
}

func TestWorkflowStripMaxDepthStatic(t *testing.T) {
	tree, wfroot, rootDepth := wfTreeWithChildren(t, 0)
	pool := WorkflowPool{MaxDepth: 1}
	// A child at depth rootDepth+1 == the limit: it may never spawn.
	atLimit := NewWorkflowStripPredicate(tree, wfroot, pool, rootDepth+1, rootDepth)
	if !atLimit() {
		t.Fatal("node at rootDepth+max_depth must be stripped (static)")
	}
	// The workflow root itself (depth rootDepth) is below the limit.
	belowLimit := NewWorkflowStripPredicate(tree, wfroot, pool, rootDepth, rootDepth)
	if belowLimit() {
		t.Fatal("workflow root below depth limit should not be stripped")
	}
}

func TestWorkflowStripZeroDimensionsNeverStrip(t *testing.T) {
	tree, wfroot, rootDepth := wfTreeWithChildren(t, 5)
	pool := WorkflowPool{} // all unset
	p := NewWorkflowStripPredicate(tree, wfroot, pool, 2, rootDepth)
	if p() {
		t.Fatal("all pool dimensions unset (0): should never strip")
	}
}

// orPredicates composes the global and workflow predicates: the tighter governs.
func TestOrPredicatesTighterWins(t *testing.T) {
	strips := func() bool { return true }
	nostrip := func() bool { return false }
	if !orPredicates(nostrip, strips)() {
		t.Fatal("or: any true => strip")
	}
	if orPredicates(nostrip, nostrip)() {
		t.Fatal("or: all false => no strip")
	}
	if orPredicates(nil, strips)() != true {
		t.Fatal("or: nil operand ignored, true still strips")
	}
	if orPredicates(nil, nil)() {
		t.Fatal("or: all nil => no strip")
	}
}
