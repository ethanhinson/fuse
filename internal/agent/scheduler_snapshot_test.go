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

func TestSnapshotImplicitPoolOnlyWhenActive(t *testing.T) {
	tr := NewAgentTreeWithConcurrency("root", "m", 4)
	sc := tr.Scheduler()

	// Idle freeform session: no children, no queue, no spend ⇒ no implicit pool.
	if snap := sc.Snapshot(); len(snap.Pools) != 0 {
		t.Errorf("idle session should have no pools, got %+v", snap.Pools)
	}

	// One running freeform child ⇒ the implicit ("") pool appears. Its
	// SlotsInUse is the scheduler's granted-slot count: a second, merely
	// PENDING (queued) child must not inflate it — a queued child holds no
	// slot, and counting it would double-report it as slot user and waiter.
	tr.addNode(&AgentNode{ID: newNodeID(), ParentID: tr.RootID(), Depth: 1, Status: StatusRunning})
	tr.addNode(&AgentNode{ID: newNodeID(), ParentID: tr.RootID(), Depth: 1, Status: StatusPending})
	if err := sc.acquireSlot(context.Background(), ""); err != nil {
		t.Fatalf("acquire slot: %v", err)
	}
	snap := sc.Snapshot()
	ok := len(snap.Pools) > 0
	var p PoolSnapshot
	if ok {
		p = snap.Pools[0]
	}
	if !ok {
		t.Fatalf("implicit pool missing once a freeform child runs: %+v", snap.Pools)
	}
	if p.SlotsInUse != 1 || p.SlotTotal != 4 {
		t.Errorf("implicit pool slots = %d/%d, want 1/4 (granted slots only, pending excluded)", p.SlotsInUse, p.SlotTotal)
	}
}

func TestSnapshotQueuedDepth(t *testing.T) {
	tr := NewAgentTreeWithConcurrency("root", "m", 1)
	sc := tr.Scheduler()

	// Occupy the one slot so any further acquire must park in the FIFO.
	if err := sc.acquireSlot(context.Background(), ""); err != nil {
		t.Fatal("initial acquire should grant on an empty cap-1 pool")
	}
	done := make(chan struct{})
	go func() {
		// This acquire blocks in the FIFO until we release.
		sc.acquireSlot(context.Background(), "")
		close(done)
	}()

	// Wait until the waiter is definitely parked.
	deadline := time.After(2 * time.Second)
	for sc.queuedDepth("") < 1 {
		select {
		case <-deadline:
			t.Fatal("waiter never parked in the FIFO")
		case <-time.After(time.Millisecond):
		}
	}

	snap := sc.Snapshot()
	if len(snap.Pools) == 0 {
		t.Fatalf("pool missing from snapshot: %+v", snap)
	}
	p := snap.Pools[0]
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
