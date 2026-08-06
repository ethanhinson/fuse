package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// The fair queue (change 0036, Task 2) replaces the bare arrival-order semaphore
// wakeup with an explicit grant dispatcher: spawn waiters land in their pool's
// FIFO, a freed slot is granted round-robin across non-empty pools (FIFO within
// a pool), per-pool pending is bounded, and unyield reacquisition jumps a
// priority lane. These tests drive that behavior at the scheduler seam.

// blockWaiter enqueues a waiter for poolID on a goroutine and reports the pool
// it was granted through a shared, ordered log once its acquire returns. The
// returned started channel closes once the goroutine is definitely blocked in
// acquireSlot (best-effort: it has entered the call).
type grantLog struct {
	mu    sync.Mutex
	order []string
}

func (g *grantLog) record(poolID string) {
	g.mu.Lock()
	g.order = append(g.order, poolID)
	g.mu.Unlock()
}

func (g *grantLog) snapshot() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]string, len(g.order))
	copy(out, g.order)
	return out
}

// TestFairQueueRoundRobinAcrossPools is spec Acceptance 2's convoy test: pool A
// enqueues 15 waiters, then pool B enqueues 3. As slots free, the dispatcher must
// interleave A and B round-robin rather than draining all of A first — so B's
// three grants must all land within the first several releases, not after A's 15.
func TestFairQueueRoundRobinAcrossPools(t *testing.T) {
	const cap = 1
	tr := NewAgentTreeWithConcurrency("root", "m", cap)
	sc := tr.Scheduler()

	// Occupy the single slot so every subsequent acquire must queue.
	if !sc.acquireSlot(context.Background(), "A") {
		t.Fatal("initial acquire should grant on an empty cap-1 pool")
	}

	log := &grantLog{}
	var wg sync.WaitGroup
	enqueue := func(pool string) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !sc.acquireSlot(context.Background(), pool) {
				t.Errorf("pool %s waiter cancelled unexpectedly", pool)
				return
			}
			log.record(pool)
			// Immediately free the slot so the next queued waiter is dispatched.
			sc.releaseSlot()
		}()
	}

	// Enqueue A's 15 first, then B's 3. Give A's goroutines a head start so they
	// are all parked in the FIFO before B arrives — this is the convoy setup.
	for i := 0; i < 15; i++ {
		enqueue("A")
	}
	if err := waitForQueueDepth(sc, "A", 15, time.Second); err != nil {
		t.Fatalf("A did not fill the queue: %v", err)
	}
	for i := 0; i < 3; i++ {
		enqueue("B")
	}
	if err := waitForQueueDepth(sc, "B", 3, time.Second); err != nil {
		t.Fatalf("B did not fill the queue: %v", err)
	}

	// Release the initial slot: the whole queue now drains one grant at a time
	// (each waiter frees its slot on completion).
	sc.releaseSlot()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("fair queue did not drain")
	}

	order := log.snapshot()
	if len(order) != 18 {
		t.Fatalf("granted %d waiters, want 18", len(order))
	}
	// Round-robin: B's last grant must arrive well before A's queue is drained.
	// With interleaving, B (3) is exhausted within the first 6 grants; a convoy
	// (drain-A-first) would push B's grants to positions 16-18.
	lastB := -1
	for i, p := range order {
		if p == "B" {
			lastB = i
		}
	}
	if lastB < 0 {
		t.Fatal("no B waiter was granted")
	}
	if lastB > 6 {
		t.Fatalf("last B grant at position %d (order=%v) — convoy, not round-robin", lastB, order)
	}
}

// TestFairQueueFIFOWithinPool asserts that within one pool, grants preserve
// enqueue order.
func TestFairQueueFIFOWithinPool(t *testing.T) {
	const cap = 1
	tr := NewAgentTreeWithConcurrency("root", "m", cap)
	sc := tr.Scheduler()
	if !sc.acquireSlot(context.Background(), "A") {
		t.Fatal("initial acquire should grant")
	}

	log := &grantLog{}
	// Enqueue waiters one at a time, waiting for each to park before the next, so
	// FIFO order is deterministic.
	labels := []string{"a0", "a1", "a2", "a3"}
	var wg sync.WaitGroup
	for i, lbl := range labels {
		lbl := lbl
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !sc.acquireSlot(context.Background(), "A") {
				t.Errorf("%s cancelled", lbl)
				return
			}
			log.record(lbl)
			sc.releaseSlot()
		}()
		if err := waitForQueueDepth(sc, "A", i+1, time.Second); err != nil {
			t.Fatalf("waiter %s did not park: %v", lbl, err)
		}
	}

	sc.releaseSlot()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("fifo queue did not drain")
	}

	order := log.snapshot()
	want := []string{"a0", "a1", "a2", "a3"}
	if len(order) != len(want) {
		t.Fatalf("order=%v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order=%v, want FIFO %v", order, want)
		}
	}
}

// TestQueueBoundOverBoundVerdict is spec Acceptance 3's admission side: once a
// pool's pending queue reaches ceil(queue_bound × slots), Admit reports OverBound
// for that pool (the answer Task 3's visibility predicate strips on), while a
// pool still under its bound is Queued.
func TestQueueBoundOverBoundVerdict(t *testing.T) {
	const cap = 2 // implicit/global pool slots figure
	tr := NewAgentTreeWithConcurrency("root", "m", cap)
	sc := tr.Scheduler()

	// Saturate the two slots so new requests would queue.
	if !sc.acquireSlot(context.Background(), "") {
		t.Fatal("acquire 1")
	}
	if !sc.acquireSlot(context.Background(), "") {
		t.Fatal("acquire 2")
	}

	// Bound = ceil(2.0 * 2) = 4. Park waiters up to the bound.
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !sc.acquireSlot(context.Background(), "") {
				return
			}
			sc.releaseSlot()
		}()
	}
	if err := waitForQueueDepth(sc, "", 4, time.Second); err != nil {
		t.Fatalf("queue did not reach the bound: %v", err)
	}

	v, err := sc.Admit(AdmitRequest{Depth: 0, PoolID: ""})
	if err != nil {
		t.Fatalf("Admit at bound err=%v", err)
	}
	if v != OverBound {
		t.Fatalf("Admit at bound verdict=%v, want OverBound", v)
	}

	// Drain everything and confirm the waiters were not leaked.
	sc.releaseSlot()
	sc.releaseSlot()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("bound-test queue did not drain")
	}
}

// TestQueueBoundUsesPoolConcurrent asserts a registered workflow pool bounds its
// pending queue on its own Concurrent figure (ceil(2.0 * concurrent)), not the
// global cap. The global slot cap must be saturated first (that is what forces
// spawns to queue at all); then a pool with a small Concurrent has a
// correspondingly small pending ceiling even though the global cap is larger.
func TestQueueBoundUsesPoolConcurrent(t *testing.T) {
	const cap = 1
	tr := NewAgentTreeWithConcurrency("root", "m", cap)
	sc := tr.Scheduler()
	root := tr.Node(tr.RootID())
	root.WorkflowRoot = "research"
	sc.RegisterPool(root.ID, WorkflowPool{Concurrent: 1})

	// Saturate the single global slot so subsequent acquires queue.
	if !sc.acquireSlot(context.Background(), "") {
		t.Fatal("hold the global slot")
	}

	// Pool slots = pool.Concurrent = 1, bound = ceil(2.0 * 1) = 2. Park 2 waiters.
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !sc.acquireSlot(context.Background(), root.ID) {
				return
			}
			sc.releaseSlot()
		}()
	}
	if err := waitForQueueDepth(sc, root.ID, 2, time.Second); err != nil {
		t.Fatalf("pool queue did not reach bound: %v", err)
	}

	v, err := sc.Admit(AdmitRequest{Depth: root.Depth, PoolID: root.ID})
	if err != nil {
		t.Fatalf("Admit err=%v", err)
	}
	if v != OverBound {
		t.Fatalf("verdict=%v, want OverBound (pool bound 2 reached)", v)
	}

	// Drain: free the held global slot; the two waiters complete in turn.
	sc.releaseSlot()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("pool bound queue did not drain")
	}
}

// TestSpawnRefusedOverBound is the call-time side of Acceptance 3: a spawn that
// races through while its pool is over-bound is denied with ErrQueueBoundExceeded
// rather than growing the queue past the bound.
func TestSpawnRefusedOverBound(t *testing.T) {
	tr := NewAgentTreeWithConcurrency("root", "m", 1)
	sc := tr.Scheduler()
	rootNode := tr.Node(tr.RootID())

	// Hold the single slot and park waiters up to the bound (ceil(2*1)=2) so the
	// implicit pool is at its ceiling.
	if !sc.acquireSlot(context.Background(), "") {
		t.Fatal("hold slot")
	}
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !sc.acquireSlot(context.Background(), "") {
				return
			}
			sc.releaseSlot()
		}()
	}
	if err := waitForQueueDepth(sc, "", 2, time.Second); err != nil {
		t.Fatalf("queue did not reach bound: %v", err)
	}

	s := NewSpawner(WithTree(tr), WithNode(rootNode), WithSpawnDepth(0))
	_, err := s.Spawn(context.Background(), SpawnOpts{Label: "over"})
	if !errors.Is(err, ErrQueueBoundExceeded) {
		t.Fatalf("racing spawn over bound err=%v, want ErrQueueBoundExceeded", err)
	}

	// Drain.
	sc.releaseSlot()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("over-bound drain hung")
	}
}

// TestReacquireLaneSkipsPoolQueue is the unyield deadlock regression at the
// scheduler seam: a resumed parent's reacquisition must jump ahead of a full
// pool FIFO. If unyield queued behind the pool's pending spawns, a parent holding
// the last logical slot for its own descendants would deadlock. Here we saturate
// the cap, park spawn waiters, then reacquire — the reacquire must win the next
// freed slot ahead of the parked waiters.
func TestReacquireLaneSkipsPoolQueue(t *testing.T) {
	tr := NewAgentTreeWithConcurrency("root", "m", 1)
	sc := tr.Scheduler()

	// The parent holds the slot, then yields it (simulating a wait on a child).
	if !sc.acquireSlot(context.Background(), "") {
		t.Fatal("parent acquire")
	}
	parent := &AgentNode{ID: newNodeID(), Depth: 1}
	// Give the parent a live slot to yield: acquire on its behalf is the one above.
	sc.YieldSlot(parent) // frees the slot

	// Now park normal spawn waiters behind the (now free) slot by first taking it.
	if !sc.acquireSlot(context.Background(), "A") {
		t.Fatal("filler acquire")
	}
	granted := make(chan string, 4)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !sc.acquireSlot(context.Background(), "A") {
				return
			}
			granted <- "spawn"
			sc.releaseSlot()
		}()
	}
	if err := waitForQueueDepth(sc, "A", 2, time.Second); err != nil {
		t.Fatalf("filler queue did not park: %v", err)
	}

	// The parent unyields (reacquires) — it must take the NEXT freed slot ahead of
	// the two parked spawn waiters, via the priority lane.
	reDone := make(chan bool, 1)
	go func() { reDone <- sc.UnyieldSlot(context.Background(), parent) }()
	// Wait until the parent is definitely parked in the priority lane before
	// freeing a slot, so the dispatch race is deterministic.
	if err := waitForPriorityDepth(sc, 1, time.Second); err != nil {
		t.Fatalf("reacquire did not park in the priority lane: %v", err)
	}

	// Free the filler's slot; the priority lane must grant the parent first.
	sc.releaseSlot()

	select {
	case ok := <-reDone:
		if !ok {
			t.Fatal("parent reacquire cancelled")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("parent reacquire deadlocked behind the pool FIFO")
	}
	select {
	case g := <-granted:
		t.Fatalf("a pool waiter (%s) was granted before the parent reacquired — priority lane broken", g)
	default:
	}

	// Let the parent finish and drain the rest.
	sc.releaseSlot() // parent gives its slot back
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("reacquire-lane drain hung")
	}
}

// TestFairQueueWaiterCancelLeavesQueue asserts a cancelled waiter removes its own
// queue entry (no leak) and takes no slot: after it cancels, a fresh acquire that
// races in still gets the freed slot rather than a phantom hold.
func TestFairQueueWaiterCancelLeavesQueue(t *testing.T) {
	tr := NewAgentTreeWithConcurrency("root", "m", 1)
	sc := tr.Scheduler()
	if !sc.acquireSlot(context.Background(), "A") {
		t.Fatal("hold slot")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancelled := make(chan bool, 1)
	go func() { cancelled <- sc.acquireSlot(ctx, "A") }()
	if err := waitForQueueDepth(sc, "A", 1, time.Second); err != nil {
		t.Fatalf("waiter did not park: %v", err)
	}

	cancel()
	select {
	case ok := <-cancelled:
		if ok {
			t.Fatal("cancelled acquire returned true (took a slot)")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancel did not unblock the waiter")
	}
	// The waiter must have left the queue.
	if err := waitForQueueDepth(sc, "A", 0, time.Second); err != nil {
		t.Fatalf("cancelled waiter leaked its queue entry: %v", err)
	}
	// The single slot is still held by the very first acquire; release it and a
	// new waiter must be granted (no phantom slot consumed by the cancelled one).
	sc.releaseSlot()
	if !sc.acquireSlot(context.Background(), "A") {
		t.Fatal("post-cancel acquire should get the freed slot")
	}
}

// waitForQueueDepth polls the scheduler's per-pool queued-waiter count until it
// equals want or the timeout elapses. It exercises a test-only accessor the
// dispatcher exposes for deterministic queue-state assertions.
func waitForQueueDepth(sc *Scheduler, poolID string, want int, timeout time.Duration) error {
	deadline := time.After(timeout)
	for {
		if sc.queuedDepth(poolID) == want {
			return nil
		}
		select {
		case <-deadline:
			return errors.New("timeout waiting for queue depth")
		case <-time.After(2 * time.Millisecond):
		}
	}
}

// waitForPriorityDepth polls the reacquisition priority lane until it holds want
// parked parents or the timeout elapses.
func waitForPriorityDepth(sc *Scheduler, want int, timeout time.Duration) error {
	deadline := time.After(timeout)
	for {
		if sc.priorityDepth() == want {
			return nil
		}
		select {
		case <-deadline:
			return errors.New("timeout waiting for priority depth")
		case <-time.After(2 * time.Millisecond):
		}
	}
}
