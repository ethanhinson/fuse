package agent

import (
	"errors"
	"testing"
)

// Task 6 — hard token quotas. The workflow pool.tokens term and the session
// token ceiling join the unified visibility predicate as PERMANENT terms (the
// per-node token counters are append-only, so once a quota is hit spawn_agent
// stays stripped in scope). The call-time backstop in Spawn mirrors them with a
// new sentinel error. These tests pin the visibility flips and their scoping.

// buildQuotaTree builds a two-workflow tree:
//
//	root(depth0)
//	 ├─ wfA(depth1)  ← workflow root, pool registered with a token quota
//	 │   └─ a1(depth2)
//	 └─ wfB(depth1)  ← sibling workflow root, its own pool
//	     └─ b1(depth2)
//
// so sibling-subtree isolation can be asserted directly.
func buildQuotaTree(t *testing.T) (tree *AgentTree, wfA, a1, wfB, b1 string) {
	t.Helper()
	tree = NewAgentTreeWithConcurrency("root", "m", 16)
	rootID := tree.RootID()

	na := &AgentNode{ID: newNodeID(), ParentID: rootID, Label: "wfA", Depth: 1, Status: StatusRunning, WorkflowRoot: "A"}
	tree.addNode(na)
	na1 := &AgentNode{ID: newNodeID(), ParentID: na.ID, Label: "a1", Depth: 2, Status: StatusRunning}
	tree.addNode(na1)
	nb := &AgentNode{ID: newNodeID(), ParentID: rootID, Label: "wfB", Depth: 1, Status: StatusRunning, WorkflowRoot: "B"}
	tree.addNode(nb)
	nb1 := &AgentNode{ID: newNodeID(), ParentID: nb.ID, Label: "b1", Depth: 2, Status: StatusRunning}
	tree.addNode(nb1)

	return tree, na.ID, na1.ID, nb.ID, nb1.ID
}

func TestVisibleWorkflowTokenQuotaFlipsPermanentInSubtreeOnly(t *testing.T) {
	tree, wfA, a1, wfB, b1 := buildQuotaTree(t)
	sc := tree.Scheduler()
	sc.RegisterPool(wfA, WorkflowPool{Tokens: 1000})
	sc.RegisterPool(wfB, WorkflowPool{Tokens: 1000})

	// Below quota: every node in both subtrees can spawn.
	tree.Node(a1).UpdateTokens(400, 200) // 600 in wfA
	if !sc.Visible(wfA) || !sc.Visible(a1) {
		t.Fatal("below the workflow token quota, wfA subtree must be visible")
	}

	// Cross the quota inside wfA only.
	tree.Node(a1).UpdateTokens(300, 200) // now 1100 >= 1000
	if sc.Visible(wfA) {
		t.Error("wfA root must be invisible once its token quota is exhausted")
	}
	if sc.Visible(a1) {
		t.Error("wfA child must be invisible once the subtree token quota is exhausted")
	}

	// Sibling isolation: wfB's subtree is untouched and stays visible.
	if !sc.Visible(wfB) {
		t.Error("sibling workflow wfB must stay visible — its quota is independent")
	}
	if !sc.Visible(b1) {
		t.Error("sibling workflow child b1 must stay visible")
	}

	// Permanence: because the counters are append-only, the flip does not revert.
	if sc.Visible(a1) {
		t.Error("workflow token-quota strip must be permanent, not reversible")
	}
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

func TestVisibleZeroQuotasDisableTermsByteIdentical(t *testing.T) {
	tree, wfA, a1, _, _ := buildQuotaTree(t)
	sc := tree.Scheduler()
	// pool.Tokens == 0 (unset) and no session ceiling set: no token term engages,
	// even with tokens charged well past any hypothetical limit.
	sc.RegisterPool(wfA, WorkflowPool{Tokens: 0})
	tree.Node(a1).UpdateTokens(9_000_000, 9_000_000)
	if !sc.Visible(wfA) || !sc.Visible(a1) {
		t.Error("zero pool.tokens and no session ceiling must not strip on token spend")
	}
	if !sc.Visible(tree.RootID()) {
		t.Error("with no session ceiling the root must stay visible regardless of spend")
	}
}

func TestSpawnBackstopWorkflowTokenQuota(t *testing.T) {
	tree, wfA, a1, _, _ := buildQuotaTree(t)
	sc := tree.Scheduler()
	sc.RegisterPool(wfA, WorkflowPool{Tokens: 500})
	tree.Node(a1).UpdateTokens(400, 200) // 600 >= 500

	// The scheduler's admission-quota check reports the exhausted token quota with
	// the ErrTokenQuotaExhausted identity for the call-time backstop.
	if err := sc.tokenQuotaDenied(a1); err == nil || !errors.Is(err, ErrTokenQuotaExhausted) {
		t.Fatalf("tokenQuotaDenied(a1) = %v, want ErrTokenQuotaExhausted identity", err)
	}
}

func TestSpawnBackstopSessionTokenCeiling(t *testing.T) {
	tree, _, a1, _, _ := buildQuotaTree(t)
	sc := tree.Scheduler()
	sc.SetSessionTokens(500)
	tree.Node(a1).UpdateTokens(400, 200) // 600 >= 500

	if err := sc.tokenQuotaDenied(a1); err == nil || !errors.Is(err, ErrTokenQuotaExhausted) {
		t.Fatalf("tokenQuotaDenied under session ceiling = %v, want ErrTokenQuotaExhausted", err)
	}
}

func TestTokenQuotaWarningWorkflowScopeInsideSubtreeOnly(t *testing.T) {
	tree, wfA, a1, wfB, b1 := buildQuotaTree(t)
	sc := tree.Scheduler()
	sc.RegisterPool(wfA, WorkflowPool{Tokens: 500})
	sc.RegisterPool(wfB, WorkflowPool{Tokens: 500})

	// Before exhaustion: no warning anywhere.
	tree.Node(a1).UpdateTokens(100, 100) // 200 < 500
	if got := sc.TokenQuotaWarning(a1); got != "" {
		t.Errorf("no warning before exhaustion, got %q", got)
	}

	// Exhaust wfA only.
	tree.Node(a1).UpdateTokens(300, 100) // now 600 >= 500
	if got := sc.TokenQuotaWarning(a1); got == "" {
		t.Error("wfA child must get a warning once its subtree token quota is exhausted")
	}
	if got := sc.TokenQuotaWarning(wfA); got == "" {
		t.Error("wfA root must get a warning once its subtree token quota is exhausted")
	}
	// Sibling scope isolation: wfB is untouched, so no warning there.
	if got := sc.TokenQuotaWarning(b1); got != "" {
		t.Errorf("sibling wfB child must NOT get wfA's warning, got %q", got)
	}
	if got := sc.TokenQuotaWarning(wfB); got != "" {
		t.Errorf("sibling wfB root must NOT get a warning, got %q", got)
	}
	// Freeform (non-workflow) scope: the root is outside any workflow subtree, so
	// a workflow-scoped quota never reaches it.
	if got := sc.TokenQuotaWarning(tree.RootID()); got != "" {
		t.Errorf("root (outside the workflow) must NOT get a workflow warning, got %q", got)
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

func TestTokenQuotaWarningNilAndUnset(t *testing.T) {
	tree, wfA, a1, _, _ := buildQuotaTree(t)
	sc := tree.Scheduler()
	sc.RegisterPool(wfA, WorkflowPool{Tokens: 0}) // unset
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
	tree, wfA, a1, _, _ := buildQuotaTree(t)
	sc := tree.Scheduler()
	sc.RegisterPool(wfA, WorkflowPool{Tokens: 1000})
	sc.SetSessionTokens(1000)
	tree.Node(a1).UpdateTokens(100, 100) // 200, under both

	if err := sc.tokenQuotaDenied(a1); err != nil {
		t.Fatalf("tokenQuotaDenied below both quotas = %v, want nil", err)
	}
}
