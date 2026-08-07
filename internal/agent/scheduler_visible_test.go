package agent

import (
	"context"
	"testing"
	"time"
)

// The unified visibility predicate (change 0036, Task 3) collapses every strip
// variant into one rule: spawn_agent is present in a node's schemas iff an
// admission request from that node's scope would currently be granted or queued
// within bound. These tests exercise Scheduler.StripPredicate / Scheduler.Visible
// against every preserved 0033/0034 term plus the new queue-bound flip.
//
// The global active-cap no longer STRIPS: reaching it now makes admission Queued
// (still visible), and the strip moves to the pool's queue bound (Acceptance 3).
// The pool-scoped terms (total/concurrent/max_depth) and the global lifetime
// budget keep their exact 0033/0034 semantics.

// setSchedulerBudget is the test helper for the permanent budget term.
func schedFor(t *testing.T, cap int) (*AgentTree, *Scheduler) {
	t.Helper()
	tr := NewAgentTreeWithConcurrency("root", "m", cap)
	return tr, tr.Scheduler()
}

func TestVisibleBudgetPermanent(t *testing.T) {
	tr, sc := schedFor(t, 16)
	sc.SetMaxSpawns(2)
	p := sc.StripPredicate(tr.RootID())
	if p() {
		t.Fatal("0/2 used: should not strip")
	}
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

func TestVisibleBudgetZeroNeverStrips(t *testing.T) {
	tr, sc := schedFor(t, 100)
	tr.addNode(&AgentNode{ID: newNodeID(), ParentID: tr.RootID(), Depth: 1, Status: StatusDone})
	p := sc.StripPredicate(tr.RootID())
	if p() {
		t.Fatal("max_spawns 0 must never strip via budget")
	}
}

// TestVisibleActiveCapQueuesNotStrips is the deliberate Task 3 behavior shift:
// with no waiters parked, saturating the global active-cap makes admission
// Queued (visible) rather than stripping. The old NewStripSpawnPredicate stripped
// here; the fair queue lets the model keep committing until the bound.
func TestVisibleActiveCapQueuesNotStrips(t *testing.T) {
	tr, sc := schedFor(t, 2)
	p := sc.StripPredicate(tr.RootID())
	if p() {
		t.Fatal("no active children, cap 2: should not strip")
	}
	// Two running children => running+pending == cap. No queue waiters yet, so a
	// new spawn would be Queued (visible), not stripped.
	tr.addNode(&AgentNode{ID: newNodeID(), ParentID: tr.RootID(), Depth: 1, Status: StatusRunning})
	tr.addNode(&AgentNode{ID: newNodeID(), ParentID: tr.RootID(), Depth: 1, Status: StatusRunning})
	if p() {
		t.Fatal("at active-cap but under queue bound: should be Queued (visible), not stripped")
	}
	v, _ := sc.Admit(AdmitRequest{Depth: 0, PoolID: ""})
	if v != Queued {
		t.Fatalf("at active-cap verdict=%v, want Queued", v)
	}
}

// TestVisibleQueueBoundFlip is Acceptance 3's core: with the global cap saturated
// and the pool's FIFO filled to ceil(queue_bound × slots), the pool's agents lose
// spawn_agent (strip); as the queue drains, the schema returns (visible again).
func TestVisibleQueueBoundFlip(t *testing.T) {
	const cap = 2 // implicit pool slots figure; bound = ceil(2.0*2) = 4
	tr, sc := schedFor(t, cap)
	p := sc.StripPredicate(tr.RootID())

	// Saturate the two slots so subsequent acquires queue.
	if err := sc.acquireSlot(context.Background(), ""); err != nil {
		t.Fatal("acquire 1")
	}
	if err := sc.acquireSlot(context.Background(), ""); err != nil {
		t.Fatal("acquire 2")
	}
	if p() {
		t.Fatal("cap saturated, empty queue: still under bound, should be visible")
	}

	// Park waiters up to the bound (4). Each frees its slot on grant so the drain
	// is one-at-a-time; while all four are parked the pool is at its ceiling.
	done := make(chan struct{})
	for i := 0; i < 4; i++ {
		go func() {
			if sc.acquireSlot(context.Background(), "") == nil {
				sc.releaseSlot()
			}
			done <- struct{}{}
		}()
	}
	if err := waitForQueueDepth(sc, "", 4, time.Second); err != nil {
		t.Fatalf("queue did not reach the bound: %v", err)
	}
	if !p() {
		t.Fatal("pool at queue bound: expected strip")
	}

	// Drain: free the two held slots; the four waiters complete in turn and the
	// queue empties. The predicate must return to visible.
	sc.releaseSlot()
	sc.releaseSlot()
	for i := 0; i < 4; i++ {
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("queue-bound drain hung")
		}
	}
	if err := waitForQueueDepth(sc, "", 0, time.Second); err != nil {
		t.Fatalf("queue did not drain: %v", err)
	}
	if p() {
		t.Fatal("queue drained: expected schema to return (visible)")
	}
}

// --- pool-scoped terms (change 0034), preserved byte-for-byte -----------------

// wfSubtree builds a workflow root under the tree root and registers its pool.
func wfSubtree(t *testing.T, sc *Scheduler, tr *AgentTree, pool WorkflowPool) (wfroot string, rootDepth int) {
	t.Helper()
	wf := &AgentNode{ID: newNodeID(), ParentID: tr.RootID(), Label: "wf", Depth: 1, Status: StatusRunning, WorkflowRoot: "research"}
	tr.addNode(wf)
	sc.RegisterPool(wf.ID, pool)
	return wf.ID, 1
}

func TestVisibleWorkflowConcurrentReversible(t *testing.T) {
	// High global cap so only the pool concurrent term can engage.
	tr, sc := schedFor(t, 100)
	wfroot, _ := wfSubtree(t, sc, tr, WorkflowPool{Concurrent: 2})
	p := sc.StripPredicate(wfroot)
	if p() {
		t.Fatal("0 active < concurrent 2: should not strip")
	}
	c1 := &AgentNode{ID: newNodeID(), ParentID: wfroot, Depth: 2, Status: StatusRunning}
	c2 := &AgentNode{ID: newNodeID(), ParentID: wfroot, Depth: 2, Status: StatusRunning}
	tr.addNode(c1)
	tr.addNode(c2)
	if !p() {
		t.Fatal("2 active == concurrent 2: should strip (reversible)")
	}
	c1.Finish(StatusDone, "")
	if p() {
		t.Fatal("1 active < concurrent 2 after finish: should NOT strip (reversible)")
	}
}

func TestVisibleWorkflowTotalPermanent(t *testing.T) {
	tr, sc := schedFor(t, 100)
	wfroot, _ := wfSubtree(t, sc, tr, WorkflowPool{Total: 2})
	p := sc.StripPredicate(wfroot)
	if p() {
		t.Fatal("0 spawned < total 2: should not strip")
	}
	tr.addNode(&AgentNode{ID: newNodeID(), ParentID: wfroot, Depth: 2, Status: StatusDone})
	tr.addNode(&AgentNode{ID: newNodeID(), ParentID: wfroot, Depth: 2, Status: StatusDone})
	if !p() {
		t.Fatal("2 spawned == total 2 with 0 active: should strip (permanent)")
	}
}

func TestVisibleWorkflowMaxDepthStatic(t *testing.T) {
	tr, sc := schedFor(t, 100)
	wfroot, rootDepth := wfSubtree(t, sc, tr, WorkflowPool{MaxDepth: 1})
	// A child at depth rootDepth+1 == the limit: it may never spawn.
	child := &AgentNode{ID: newNodeID(), ParentID: wfroot, Depth: rootDepth + 1, Status: StatusRunning}
	tr.addNode(child)
	if !sc.StripPredicate(child.ID)() {
		t.Fatal("node at rootDepth+max_depth must be stripped (static)")
	}
	// The workflow root itself (depth rootDepth) is below the limit.
	if sc.StripPredicate(wfroot)() {
		t.Fatal("workflow root below depth limit should not be stripped")
	}
}

func TestVisibleWorkflowZeroDimensionsNeverStrip(t *testing.T) {
	tr, sc := schedFor(t, 100)
	wfroot, _ := wfSubtree(t, sc, tr, WorkflowPool{}) // all unset
	for i := 0; i < 5; i++ {
		tr.addNode(&AgentNode{ID: newNodeID(), ParentID: wfroot, Depth: 2, Status: StatusRunning})
	}
	if sc.StripPredicate(wfroot)() {
		t.Fatal("all pool dimensions unset (0): should never strip")
	}
}

// TestVisibleSiblingSubtreeIsolation asserts a pool's concurrent exhaustion
// strips inside that subtree only — a sibling workflow subtree at its own
// (unexhausted) pool stays visible.
func TestVisibleSiblingSubtreeIsolation(t *testing.T) {
	tr, sc := schedFor(t, 100)
	// Two sibling workflow roots, each with Concurrent 1.
	a := &AgentNode{ID: newNodeID(), ParentID: tr.RootID(), Depth: 1, Status: StatusRunning, WorkflowRoot: "research"}
	b := &AgentNode{ID: newNodeID(), ParentID: tr.RootID(), Depth: 1, Status: StatusRunning, WorkflowRoot: "research"}
	tr.addNode(a)
	tr.addNode(b)
	sc.RegisterPool(a.ID, WorkflowPool{Concurrent: 1})
	sc.RegisterPool(b.ID, WorkflowPool{Concurrent: 1})
	// Saturate A's pool only.
	tr.addNode(&AgentNode{ID: newNodeID(), ParentID: a.ID, Depth: 2, Status: StatusRunning})
	if !sc.StripPredicate(a.ID)() {
		t.Fatal("pool A at concurrent 1: should strip")
	}
	if sc.StripPredicate(b.ID)() {
		t.Fatal("pool B untouched: must stay visible (sibling isolation)")
	}
}

func TestVisibleNilSchedulerNeverStrips(t *testing.T) {
	var sc *Scheduler
	if sc.StripPredicate("anything")() {
		t.Fatal("nil scheduler predicate should never strip")
	}
}

// TestVisibleTighterOfGlobalAndPool asserts the unified predicate still honors
// the tighter of global and workflow scope: an exhausted GLOBAL budget strips a
// node even when its pool is well under its own limits.
func TestVisibleTighterOfGlobalAndPool(t *testing.T) {
	tr, sc := schedFor(t, 100)
	sc.SetMaxSpawns(1)
	wfroot, _ := wfSubtree(t, sc, tr, WorkflowPool{Concurrent: 8, Total: 8})
	// One committed child exhausts the global budget (root excluded).
	tr.addNode(&AgentNode{ID: newNodeID(), ParentID: wfroot, Depth: 2, Status: StatusDone})
	if !sc.StripPredicate(wfroot)() {
		t.Fatal("global budget exhausted: tighter global scope must strip even under pool limits")
	}
}
