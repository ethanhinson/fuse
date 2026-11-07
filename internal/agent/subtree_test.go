package agent

import "testing"

// buildSubtreeTree constructs:
//
//	root(depth0)
//	 ├─ wfroot(depth1)
//	 │   ├─ a(depth2)
//	 │   └─ b(depth2)
//	 │       └─ c(depth3)
//	 └─ sibling(depth1)
//
// and returns the tree plus the ids that matter.
func buildSubtreeTree(t *testing.T) (tree *AgentTree, wfroot, a, b, c, sibling string) {
	t.Helper()
	tree = NewAgentTree("root", "m")
	rootID := tree.RootID()

	wf := &AgentNode{ID: newNodeID(), ParentID: rootID, Label: "wfroot", Depth: 1, Status: StatusRunning}
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
