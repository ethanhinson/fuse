package agent

import "testing"

// buildSubtreeTree constructs:
//
//	root(depth0)
//	 ├─ wfroot(depth1)      ← tagged workflow root "research"
//	 │   ├─ a(depth2)
//	 │   └─ b(depth2)
//	 │       └─ c(depth3)
//	 └─ sibling(depth1)     ← outside the workflow subtree
//
// and returns the tree plus the ids that matter.
func buildSubtreeTree(t *testing.T) (tree *AgentTree, wfroot, a, b, c, sibling string) {
	t.Helper()
	tree = NewAgentTree("root", "m")
	rootID := tree.RootID()

	wf := &AgentNode{ID: newNodeID(), ParentID: rootID, Label: "wfroot", Depth: 1, Status: StatusRunning, WorkflowRoot: "research"}
	tree.addNode(wf)
	na := &AgentNode{ID: newNodeID(), ParentID: wf.ID, Label: "a", Depth: 2, Status: StatusRunning}
	tree.addNode(na)
	nb := &AgentNode{ID: newNodeID(), ParentID: wf.ID, Label: "b", Depth: 2, Status: StatusPending}
	tree.addNode(nb)
	nc := &AgentNode{ID: newNodeID(), ParentID: nb.ID, Label: "c", Depth: 3, Status: StatusRunning}
	tree.addNode(nc)
	sib := &AgentNode{ID: newNodeID(), ParentID: rootID, Label: "sibling", Depth: 1, Status: StatusRunning}
	tree.addNode(sib)

	return tree, wf.ID, na.ID, nb.ID, nc.ID, sib.ID
}

func TestInSubtree(t *testing.T) {
	tree, wfroot, a, b, c, sibling := buildSubtreeTree(t)

	if !tree.InSubtree(wfroot, a) {
		t.Error("a should be in wfroot's subtree")
	}
	if !tree.InSubtree(wfroot, c) {
		t.Error("c (grandchild) should be in wfroot's subtree")
	}
	if !tree.InSubtree(wfroot, wfroot) {
		t.Error("the root itself is in its own subtree")
	}
	if tree.InSubtree(wfroot, sibling) {
		t.Error("sibling must NOT be in wfroot's subtree")
	}
	_ = b
}

func TestSubtreeActiveCounts(t *testing.T) {
	tree, wfroot, _, _, _, _ := buildSubtreeTree(t)

	// Under wfroot: a(running), b(pending), c(running) => running=2, pending=1.
	// The workflow root node itself is excluded (it's the holder, like Depth==0
	// is excluded from the global ActiveCounts).
	running, pending := tree.SubtreeActiveCounts(wfroot)
	if running != 2 || pending != 1 {
		t.Errorf("SubtreeActiveCounts = (%d,%d), want (2,1)", running, pending)
	}
}

func TestSubtreeSpawnCount(t *testing.T) {
	tree, wfroot, _, _, _, _ := buildSubtreeTree(t)

	// Nodes created under wfroot: a, b, c => 3. The root marker node is excluded.
	if got := tree.SubtreeSpawnCount(wfroot); got != 3 {
		t.Errorf("SubtreeSpawnCount = %d, want 3", got)
	}
}

func TestSubtreeTokens(t *testing.T) {
	tree, wfroot, a, b, c, sibling := buildSubtreeTree(t)

	// Charge tokens across the tree. UpdateTokens increments TokensIn/TokensOut.
	tree.Node(wfroot).UpdateTokens(100, 10) // the workflow root itself — excluded
	tree.Node(a).UpdateTokens(200, 20)      // in subtree
	tree.Node(b).UpdateTokens(300, 30)      // in subtree
	tree.Node(c).UpdateTokens(400, 40)      // in subtree (deep, grandchild)
	tree.Node(sibling).UpdateTokens(999, 99) // outside the subtree — excluded

	// SubtreeTokens excludes the root marker node itself (like SubtreeSpawnCount)
	// and sums TokensIn+TokensOut over every descendant, including deep ones.
	want := (200 + 20) + (300 + 30) + (400 + 40)
	if got := tree.SubtreeTokens(wfroot); got != want {
		t.Errorf("SubtreeTokens(wfroot) = %d, want %d", got, want)
	}
}

func TestSessionTokensWholeTree(t *testing.T) {
	tree, wfroot, a, b, c, sibling := buildSubtreeTree(t)
	rootID := tree.RootID()

	tree.Node(rootID).UpdateTokens(50, 5) // the tree root — INCLUDED in session total
	tree.Node(wfroot).UpdateTokens(100, 10)
	tree.Node(a).UpdateTokens(200, 20)
	tree.Node(b).UpdateTokens(300, 30)
	tree.Node(c).UpdateTokens(400, 40)
	tree.Node(sibling).UpdateTokens(999, 99)

	// SessionTokens sums every node's in+out across the whole tree, root included.
	want := (50 + 5) + (100 + 10) + (200 + 20) + (300 + 30) + (400 + 40) + (999 + 99)
	if got := tree.SessionTokens(); got != want {
		t.Errorf("SessionTokens = %d, want %d", got, want)
	}
}

func TestWorkflowRootOf(t *testing.T) {
	tree, wfroot, a, _, c, sibling := buildSubtreeTree(t)

	if got := tree.WorkflowRootOf(c); got != wfroot {
		t.Errorf("WorkflowRootOf(c) = %q, want wfroot %q", got, wfroot)
	}
	if got := tree.WorkflowRootOf(a); got != wfroot {
		t.Errorf("WorkflowRootOf(a) = %q, want wfroot %q", got, wfroot)
	}
	if got := tree.WorkflowRootOf(wfroot); got != wfroot {
		t.Errorf("WorkflowRootOf(wfroot) = %q, want itself", got)
	}
	if got := tree.WorkflowRootOf(sibling); got != "" {
		t.Errorf("WorkflowRootOf(sibling) = %q, want empty (not in a workflow)", got)
	}
}
