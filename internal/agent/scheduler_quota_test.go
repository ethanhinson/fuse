package agent

import (
	"errors"
	"testing"
)

// Hard token quotas. The session token ceiling joins the unified visibility predicate
// as a PERMANENT term (the per-node token counters are append-only, so once it is hit
// spawn_agent stays stripped). The call-time backstop in Spawn mirrors it with a
// sentinel error.

// buildQuotaTree builds a small tree:
//
//	root(depth0)
//	 ├─ wfA(depth1)
//	 │   └─ a1(depth2)
//	 └─ wfB(depth1)
//	     └─ b1(depth2)
func buildQuotaTree(t *testing.T) (tree *AgentTree, wfA, a1, wfB, b1 string) {
	t.Helper()
	tree = NewAgentTreeWithConcurrency("root", "m", 16)
	rootID := tree.RootID()

	na := &AgentNode{ID: newNodeID(), ParentID: rootID, Label: "wfA", Depth: 1, Status: StatusRunning}
	tree.addNode(na)
	na1 := &AgentNode{ID: newNodeID(), ParentID: na.ID, Label: "a1", Depth: 2, Status: StatusRunning}
	tree.addNode(na1)
	nb := &AgentNode{ID: newNodeID(), ParentID: rootID, Label: "wfB", Depth: 1, Status: StatusRunning}
	tree.addNode(nb)
	nb1 := &AgentNode{ID: newNodeID(), ParentID: nb.ID, Label: "b1", Depth: 2, Status: StatusRunning}
	tree.addNode(nb1)

	return tree, na.ID, na1.ID, nb.ID, nb1.ID
}

func TestVisibleSessionTokenCeilingFlipsGlobally(t *testing.T) {
	tree, wfA, a1, wfB, b1 := buildQuotaTree(t)
	sc := tree.Scheduler()
	sc.SetSessionTokens(1000)

	// Charge tokens split across both workflows; session total = whole-tree sum.
	tree.Node(a1).UpdateTokens(300, 100) // 400
	tree.Node(b1).UpdateTokens(300, 100) // +400 = 800 total
	if !sc.Visible(wfA) || !sc.Visible(wfB) {
		t.Fatal("below the session ceiling, all subtrees must be visible")
	}

	// Push the SESSION total over — the tokens land in wfB, but the strip is global.
	tree.Node(b1).UpdateTokens(200, 100) // +300 = 1100 total >= 1000
	if sc.Visible(wfA) || sc.Visible(a1) {
		t.Error("session-ceiling exhaustion must strip spawn_agent globally, incl. wfA")
	}
	if sc.Visible(wfB) || sc.Visible(b1) {
		t.Error("session-ceiling exhaustion must strip spawn_agent in wfB too")
	}
	// A freeform (non-workflow) node under the root is stripped too.
	if sc.Visible(tree.RootID()) {
		t.Error("session-ceiling exhaustion must strip the root/freeform scope too")
	}
}

func TestSpawnBackstopSessionTokenCeiling(t *testing.T) {
	tree, _, a1, _, _ := buildQuotaTree(t)
	sc := tree.Scheduler()
	sc.SetSessionTokens(500)
	tree.Node(a1).UpdateTokens(400, 200) // 600 >= 500

	if err := sc.TokenQuotaDenied(a1); err == nil || !errors.Is(err, ErrTokenQuotaExhausted) {
		t.Fatalf("TokenQuotaDenied under session ceiling = %v, want ErrTokenQuotaExhausted", err)
	}
}

func TestTokenQuotaWarningSessionScopeGlobal(t *testing.T) {
	tree, wfA, a1, wfB, b1 := buildQuotaTree(t)
	sc := tree.Scheduler()
	sc.SetSessionTokens(500)

	tree.Node(a1).UpdateTokens(400, 200) // 600 total >= 500
	// Session scope: EVERY node gets the warning, in and out of any workflow.
	for _, id := range []string{a1, wfA, b1, wfB, tree.RootID()} {
		if got := sc.TokenQuotaWarning(id); got == "" {
			t.Errorf("session ceiling exhausted: node %q must get a warning", id)
		}
	}
}

func TestVisibleZeroQuotasDisableTermsByteIdentical(t *testing.T) {
	tree, wfA, a1, _, _ := buildQuotaTree(t)
	sc := tree.Scheduler()
	// No session ceiling set: no token term engages, even with tokens charged well
	// past any hypothetical limit.
	tree.Node(a1).UpdateTokens(9_000_000, 9_000_000)
	if !sc.Visible(wfA) || !sc.Visible(a1) {
		t.Error("no session ceiling must not strip on token spend")
	}
	if !sc.Visible(tree.RootID()) {
		t.Error("with no session ceiling the root must stay visible regardless of spend")
	}
}

func TestTokenQuotaWarningNilAndUnset(t *testing.T) {
	tree, _, a1, _, _ := buildQuotaTree(t)
	sc := tree.Scheduler()
	tree.Node(a1).UpdateTokens(9_000_000, 9_000_000)
	if got := sc.TokenQuotaWarning(a1); got != "" {
		t.Errorf("unset quotas => no warning, got %q", got)
	}
	var nilSched *Scheduler
	if got := nilSched.TokenQuotaWarning("x"); got != "" {
		t.Errorf("nil scheduler => no warning, got %q", got)
	}
}

func TestTokenQuotaDeniedNilBelow(t *testing.T) {
	tree, _, a1, _, _ := buildQuotaTree(t)
	sc := tree.Scheduler()
	sc.SetSessionTokens(1000)
	tree.Node(a1).UpdateTokens(100, 100) // 200, under the ceiling

	if err := sc.TokenQuotaDenied(a1); err != nil {
		t.Fatalf("TokenQuotaDenied below the ceiling = %v, want nil", err)
	}
}
