package agent

import (
	"context"
	"testing"
	"time"
)

// Snapshot (change 0036, Task 7) exposes the scheduler's global and per-pool
// observability counters as a race-safe copy for the status bar / agents view.
// These tests construct a tree + pool + queue state and assert the copy reflects
// it exactly.

// findPool returns the PoolSnapshot for poolID (workflow root node ID, or "" for
// the implicit session pool), and whether it was present.
func findPool(snap SchedulerSnapshot, poolID string) (PoolSnapshot, bool) {
	for _, p := range snap.Pools {
		if p.PoolID == poolID {
			return p, true
		}
	}
	return PoolSnapshot{}, false
}

func TestSnapshotGlobalCounters(t *testing.T) {
	tr := NewAgentTreeWithConcurrency("root", "m", 4)
	sc := tr.Scheduler()
	sc.SetMaxSpawns(10)
	sc.SetSessionTokens(1000)

	// Three child nodes (budget used = 3) with token spend, plus three slots
	// actually granted through the scheduler. The global SlotsInUse is the
	// scheduler's own granted-slot count (N-2), not the tree's running+pending —
	// so slots-in-use is driven by acquireSlot, not node Status.
	tr.addNode(&AgentNode{ID: newNodeID(), ParentID: tr.RootID(), Depth: 1, Status: StatusRunning})
	tr.addNode(&AgentNode{ID: newNodeID(), ParentID: tr.RootID(), Depth: 1, Status: StatusRunning})
	c3 := newNodeID()
	tr.addNode(&AgentNode{ID: c3, ParentID: tr.RootID(), Depth: 1, Status: StatusPending})
	tr.Node(c3).UpdateTokens(200, 100) // session spend 300
	for i := 0; i < 3; i++ {
		if err := sc.acquireSlot(context.Background(), ""); err != nil {
			t.Fatalf("acquire slot %d: %v", i, err)
		}
	}

	snap := sc.Snapshot()
	if snap.SlotsInUse != 3 {
		t.Errorf("SlotsInUse = %d, want 3 (scheduler-granted slots, not tree pending)", snap.SlotsInUse)
	}
	if snap.SlotTotal != 4 {
		t.Errorf("SlotTotal = %d, want 4", snap.SlotTotal)
	}
	if snap.BudgetUsed != 3 || snap.BudgetMax != 10 {
		t.Errorf("budget = %d/%d, want 3/10", snap.BudgetUsed, snap.BudgetMax)
	}
	if snap.SessionTokens != 300 || snap.SessionCeiling != 1000 {
		t.Errorf("session tokens = %d/%d, want 300/1000", snap.SessionTokens, snap.SessionCeiling)
	}
}

func TestSnapshotPerPoolCounters(t *testing.T) {
	tr := NewAgentTreeWithConcurrency("root", "m", 8)
	sc := tr.Scheduler()

	// A registered workflow pool rooted at a depth-1 node.
	wf := &AgentNode{ID: newNodeID(), ParentID: tr.RootID(), Label: "wf", Depth: 1, Status: StatusRunning, WorkflowRoot: "research"}
	tr.addNode(wf)
	sc.RegisterPool(wf.ID, WorkflowPool{Concurrent: 5, Total: 20, Tokens: 500})

	// Two children under the pool root: one running, one pending, with token spend.
	// SubtreeActiveCounts/SubtreeTokens exclude the pool-root holder itself.
	ca := newNodeID()
	cb := newNodeID()
	tr.addNode(&AgentNode{ID: ca, ParentID: wf.ID, Depth: 2, Status: StatusRunning})
	tr.addNode(&AgentNode{ID: cb, ParentID: wf.ID, Depth: 2, Status: StatusPending})
	tr.Node(ca).UpdateTokens(200, 110) // subtree spend 310

	snap := sc.Snapshot()
	p, ok := findPool(snap, wf.ID)
	if !ok {
		t.Fatalf("workflow pool %q missing from snapshot: %+v", wf.ID, snap.Pools)
	}
	if p.Workflow != "research" {
		t.Errorf("pool Workflow = %q, want research", p.Workflow)
	}
	if p.SlotsInUse != 2 {
		t.Errorf("pool SlotsInUse = %d, want 2", p.SlotsInUse)
	}
	if p.SlotTotal != 5 {
		t.Errorf("pool SlotTotal = %d, want 5", p.SlotTotal)
	}
	if p.TokenSpend != 310 || p.TokenQuota != 500 {
		t.Errorf("pool tokens = %d/%d, want 310/500", p.TokenSpend, p.TokenQuota)
	}
}

func TestSnapshotQueuedDepth(t *testing.T) {
	tr := NewAgentTreeWithConcurrency("root", "m", 1)
	sc := tr.Scheduler()

	// Occupy the one slot so any further acquire must park in its pool's FIFO.
	if err := sc.acquireSlot(context.Background(), "research"); err != nil {
		t.Fatal("initial acquire should grant on an empty cap-1 pool")
	}
	done := make(chan struct{})
	go func() {
		// This acquire blocks in the "research" pool FIFO until we release.
		sc.acquireSlot(context.Background(), "research")
		close(done)
	}()

	// Wait until the waiter is definitely parked.
	deadline := time.After(2 * time.Second)
	for sc.queuedDepth("research") < 1 {
		select {
		case <-deadline:
			t.Fatal("waiter never parked in the research pool FIFO")
		case <-time.After(time.Millisecond):
		}
	}

	// The pool need not be registered to appear in the queue; register it so the
	// snapshot surfaces it as a pool with a queued depth.
	wf := &AgentNode{ID: "research", ParentID: tr.RootID(), Depth: 1, Status: StatusRunning, WorkflowRoot: "research"}
	tr.addNode(wf)
	sc.RegisterPool("research", WorkflowPool{Concurrent: 1})

	snap := sc.Snapshot()
	p, ok := findPool(snap, "research")
	if !ok {
		t.Fatalf("research pool missing from snapshot: %+v", snap.Pools)
	}
	if p.Queued != 1 {
		t.Errorf("pool Queued = %d, want 1", p.Queued)
	}

	// Release both slots so the parked waiter completes and the goroutine exits.
	sc.releaseSlot()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("parked waiter never granted after release")
	}
	sc.releaseSlot()
}

func TestSnapshotImplicitPoolOnlyWhenActive(t *testing.T) {
	tr := NewAgentTreeWithConcurrency("root", "m", 4)
	sc := tr.Scheduler()

	// Idle freeform session: no children, no queue, no spend ⇒ no implicit pool.
	if snap := sc.Snapshot(); len(snap.Pools) != 0 {
		t.Errorf("idle session should have no pools, got %+v", snap.Pools)
	}

	// One running freeform child ⇒ the implicit ("") pool appears.
	tr.addNode(&AgentNode{ID: newNodeID(), ParentID: tr.RootID(), Depth: 1, Status: StatusRunning})
	snap := sc.Snapshot()
	p, ok := findPool(snap, "")
	if !ok {
		t.Fatalf("implicit pool missing once a freeform child runs: %+v", snap.Pools)
	}
	if p.SlotsInUse != 1 || p.SlotTotal != 4 {
		t.Errorf("implicit pool slots = %d/%d, want 1/4", p.SlotsInUse, p.SlotTotal)
	}
}
