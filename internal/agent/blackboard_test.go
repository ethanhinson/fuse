package agent

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"
)

// --- Task 1: store Put/Get/Delete/Keys/Snapshot ---------------------------------

func TestBlackboardPutGetProvenance(t *testing.T) {
	bb := NewBlackboard(nil)
	bb.Put("k", 42, "node-1", "research/facet-2")

	e, ok := bb.Get("k")
	if !ok {
		t.Fatal("Get missing key just written")
	}
	if e.Value != 42 {
		t.Fatalf("value = %v, want 42", e.Value)
	}
	if e.WriterID != "node-1" || e.WriterLabel != "research/facet-2" {
		t.Fatalf("provenance = %q/%q, want node-1/research/facet-2", e.WriterID, e.WriterLabel)
	}
	if e.WrittenAt.IsZero() {
		t.Fatal("WrittenAt not stamped")
	}
}

func TestBlackboardGetMissing(t *testing.T) {
	bb := NewBlackboard(nil)
	e, ok := bb.Get("nope")
	if ok {
		t.Fatalf("Get on missing key returned ok=true (%+v)", e)
	}
	if e.Value != nil {
		t.Fatalf("missing Get returned non-zero value %v", e.Value)
	}
}

func TestBlackboardLastWriterWins(t *testing.T) {
	bb := NewBlackboard(nil)
	bb.Put("k", "first", "a", "A")
	bb.Put("k", "second", "b", "B")
	e, _ := bb.Get("k")
	if e.Value != "second" || e.WriterID != "b" || e.WriterLabel != "B" {
		t.Fatalf("last-writer-wins failed: %+v", e)
	}
}

func TestBlackboardDelete(t *testing.T) {
	bb := NewBlackboard(nil)
	bb.Put("k", 1, "a", "A")
	bb.Delete("k")
	if _, ok := bb.Get("k"); ok {
		t.Fatal("key still present after Delete")
	}
	// Idempotent.
	bb.Delete("k")
	bb.Delete("never")
}

func TestBlackboardKeysGlob(t *testing.T) {
	bb := NewBlackboard(nil)
	bb.Put("foo/a", 1, "", "")
	bb.Put("foo/b", 2, "", "")
	bb.Put("bar/c", 3, "", "")
	bb.Put("baz", 4, "", "")

	cases := []struct {
		pattern string
		want    []string
	}{
		{"foo/a", []string{"foo/a"}},        // exact
		{"foo/*", []string{"foo/a", "foo/b"}}, // prefix glob
		{"*", []string{"bar/c", "baz", "foo/a", "foo/b"}},
		{"", []string{"bar/c", "baz", "foo/a", "foo/b"}},
		{"nope/*", nil}, // no match
	}
	for _, c := range cases {
		got := bb.Keys(c.pattern)
		want := c.want
		sort.Strings(want)
		if len(got) == 0 && len(want) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Keys(%q) = %v, want %v", c.pattern, got, want)
		}
		// Result must be sorted.
		if !sort.StringsAreSorted(got) {
			t.Errorf("Keys(%q) not sorted: %v", c.pattern, got)
		}
	}
}

func TestBlackboardSnapshotIndependent(t *testing.T) {
	bb := NewBlackboard(nil)
	bb.Put("k", "v", "a", "A")
	snap := bb.Snapshot()

	// Mutating the returned map must not affect the store.
	snap["k"] = BlackboardEntry{Value: "tampered"}
	snap["new"] = BlackboardEntry{Value: "x"}
	delete(snap, "k")

	e, ok := bb.Get("k")
	if !ok || e.Value != "v" {
		t.Fatalf("store mutated via snapshot: %+v ok=%v", e, ok)
	}
	if _, ok := bb.Get("new"); ok {
		t.Fatal("snapshot insertion leaked into store")
	}
}

func TestBlackboardConcurrentPutGetRace(t *testing.T) {
	bb := NewBlackboard(nil)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		i := i
		wg.Add(2)
		go func() {
			defer wg.Done()
			bb.Put(fmt.Sprintf("k%d", i%8), i, "w", "W")
		}()
		go func() {
			defer wg.Done()
			_, _ = bb.Get(fmt.Sprintf("k%d", i%8))
			_ = bb.Keys("k*")
			_ = bb.Snapshot()
		}()
	}
	wg.Wait()
}

// --- Task 2: Wait value/timeout/cancel/no-lost-wakeup ---------------------------

func TestWaitImmediateWhenSet(t *testing.T) {
	bb := NewBlackboard(nil)
	bb.Put("k", "ready", "a", "A")
	v, err := bb.Wait(context.Background(), "k", time.Second, nil)
	if err != nil {
		t.Fatalf("Wait err = %v", err)
	}
	if v != "ready" {
		t.Fatalf("Wait value = %v, want ready", v)
	}
}

func TestWaitLatePut(t *testing.T) {
	bb := NewBlackboard(nil)
	done := make(chan struct{})
	var got any
	var werr error
	go func() {
		got, werr = bb.Wait(context.Background(), "k", 2*time.Second, nil)
		close(done)
	}()
	// Give the waiter time to block, then write.
	time.Sleep(20 * time.Millisecond)
	bb.Put("k", "late", "a", "A")

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not wake on late Put")
	}
	if werr != nil {
		t.Fatalf("Wait err = %v", werr)
	}
	if got != "late" {
		t.Fatalf("Wait value = %v, want late", got)
	}
}

func TestWaitTimeout(t *testing.T) {
	bb := NewBlackboard(nil)
	start := time.Now()
	v, err := bb.Wait(context.Background(), "never", 30*time.Millisecond, nil)
	if err == nil {
		t.Fatal("Wait on never-set key returned nil error")
	}
	if v != nil {
		t.Fatalf("timed-out Wait returned value %v", v)
	}
	if time.Since(start) < 25*time.Millisecond {
		t.Fatal("Wait returned before the timeout elapsed")
	}
}

func TestWaitContextCancel(t *testing.T) {
	bb := NewBlackboard(nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := bb.Wait(ctx, "never", time.Minute, nil)
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("Wait err = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Wait did not return on ctx cancel")
	}
}

// TestWaitNoLostWakeup stresses the register-before-recheck guard: a Put that
// races the waiter's registration must still wake it. Under -race this also
// exercises the lock discipline around waiters.
func TestWaitNoLostWakeup(t *testing.T) {
	for i := 0; i < 200; i++ {
		bb := NewBlackboard(nil)
		key := "k"
		got := make(chan any, 1)
		go func() {
			v, err := bb.Wait(context.Background(), key, time.Second, nil)
			if err != nil {
				got <- err
				return
			}
			got <- v
		}()
		// Race the Put against the registration.
		bb.Put(key, "v", "a", "A")
		select {
		case v := <-got:
			if v != "v" {
				t.Fatalf("iter %d: lost wakeup or wrong value: %v", i, v)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("iter %d: lost wakeup — Wait never returned", i)
		}
	}
}

// TestWaitSlotYieldSaturationRegression is the highest-risk deadlock regression
// (spec D1). With a real tree at cap 2, both slots are held by non-root nodes: a
// consumer and one producer. A second producer is blocked awaiting a slot to
// write the key. The consumer's Wait must YIELD its slot while blocked — freeing
// the slot the second producer needs — then be woken by that producer's Put and
// reacquire. Without the yield, the pool is starved and the whole thing
// deadlocks. It must complete within the bounded test timeout.
func TestWaitSlotYieldSaturationRegression(t *testing.T) {
	const cap = 2
	tr := NewAgentTreeWithConcurrency("root", "m", cap)
	sc := tr.Scheduler()
	bb := NewBlackboard(tr)

	// Two non-root nodes (depth >= 1) so YieldSlot/UnyieldSlot are not no-ops.
	consumer := &AgentNode{ID: newNodeID(), Depth: 1}
	producer := &AgentNode{ID: newNodeID(), Depth: 1}

	// Saturate the cap: consumer holds one slot, producer holds the other.
	if err := sc.acquireSlot(context.Background(), ""); err != nil {
		t.Fatal("consumer acquire")
	}
	if err := sc.acquireSlot(context.Background(), ""); err != nil {
		t.Fatal("producer acquire")
	}
	if snap := sc.Snapshot(); snap.SlotsInUse != cap {
		t.Fatalf("expected saturated cap: SlotsInUse=%d, want %d", snap.SlotsInUse, cap)
	}

	// A SECOND producer that cannot proceed until a slot frees; when it gets one it
	// writes the key the consumer waits on. This models a producer queued behind the
	// saturated cap.
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		if err := sc.acquireSlot(context.Background(), ""); err != nil {
			return
		}
		bb.Put("result", "produced", producer.ID, "producer")
		sc.releaseSlot()
	}()

	// The consumer blocks on the key. Wait must yield the consumer's slot, letting
	// the blocked writer acquire it and produce the value.
	waitDone := make(chan struct{})
	var got any
	var werr error
	go func() {
		defer close(waitDone)
		got, werr = bb.Wait(context.Background(), "result", 5*time.Second, consumer)
	}()

	select {
	case <-waitDone:
	case <-time.After(10 * time.Second):
		t.Fatal("DEADLOCK: consumer Wait never completed — slot not yielded")
	}
	if werr != nil {
		t.Fatalf("consumer Wait err = %v", werr)
	}
	if got != "produced" {
		t.Fatalf("consumer got %v, want produced", got)
	}
	<-writerDone

	// Release the producer's and consumer's (reacquired) slots.
	sc.releaseSlot() // producer's original slot
	sc.releaseSlot() // consumer's reacquired slot
}

// TestWaitFastPathSkipsYield (second-pass audit F3): a Wait on an ALREADY-SET key
// returns via the fast path BEFORE the yield block, so it must NOT release then
// reacquire the caller's scheduler slot. A spurious yield here would transiently
// free a slot and let an over-cap child in. Assert SlotsInUse is unchanged.
func TestWaitFastPathSkipsYield(t *testing.T) {
	tr := NewAgentTreeWithConcurrency("root", "m", 2)
	sc := tr.Scheduler()
	bb := NewBlackboard(tr)
	node := &AgentNode{ID: newNodeID(), Depth: 1} // depth>=1 so yield is not a no-op

	if err := sc.acquireSlot(context.Background(), ""); err != nil {
		t.Fatal("acquire")
	}
	before := sc.Snapshot().SlotsInUse

	bb.Put("k", "ready", "i", "l") // key present => fast path
	v, err := bb.Wait(context.Background(), "k", time.Second, node)
	if err != nil || v != "ready" {
		t.Fatalf("fast-path Wait = (%v, %v), want (ready, nil)", v, err)
	}
	if after := sc.Snapshot().SlotsInUse; after != before {
		t.Fatalf("fast path must not yield: SlotsInUse %d -> %d", before, after)
	}
	sc.releaseSlot()
}

// TestWaitReacquireOnCancelledCtx (second-pass audit F4, corrected): when Wait
// blocks (yielding its slot) and the ctx is then cancelled, the deferred
// UnyieldSlot reacquires with the already-cancelled ctx. The finder predicted the
// reacquire would FAIL and leave the slot released — but that is WRONG when a slot
// is free: reacquireSlot's enqueueSlot grants IMMEDIATELY (no await, so the
// cancelled ctx never matters), and SlotsInUse returns to its pre-wait value. The
// ctx only gates reacquire when it must QUEUE behind a busy slot. This test pins
// the actual (correct) free-slot behavior; TestWaitReacquireBlockedByBusySlot
// below covers the genuinely-contended path where cancellation does drop the slot.
func TestWaitReacquireOnCancelledCtx(t *testing.T) {
	tr := NewAgentTreeWithConcurrency("root", "m", 1)
	sc := tr.Scheduler()
	bb := NewBlackboard(tr)
	node := &AgentNode{ID: newNodeID(), Depth: 1}

	if err := sc.acquireSlot(context.Background(), ""); err != nil {
		t.Fatal("acquire")
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := bb.Wait(ctx, "never", time.Minute, node) // blocks => yields the slot
		done <- err
	}()

	// Wait for the yield to land (SlotsInUse drops to 0).
	deadline := time.Now().Add(2 * time.Second)
	for sc.Snapshot().SlotsInUse != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := sc.Snapshot().SlotsInUse; got != 0 {
		t.Fatalf("Wait did not yield the slot: SlotsInUse=%d, want 0", got)
	}

	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("Wait err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not return on cancel")
	}
	// The slot IS reclaimed: it was free, so reacquire granted immediately despite
	// the cancelled ctx. SlotsInUse returns to 1.
	if got := sc.Snapshot().SlotsInUse; got != 1 {
		t.Fatalf("free-slot reacquire should reclaim despite cancel: SlotsInUse=%d, want 1", got)
	}
	sc.releaseSlot()
}

// TestWaitReacquireBlockedByBusySlot (second-pass audit F4, contended variant):
// the ONLY case where a cancelled reacquire drops the slot — the consumer yields
// its slot, another party grabs the now-free slot, and THEN the consumer's Wait
// is cancelled, so its reacquire must QUEUE and the cancelled ctx makes it fail;
// the consumer ends without a slot (SlotsInUse stays at the cap held by the other
// party). Confirms the ctx-gated reacquire path.
func TestWaitReacquireBlockedByBusySlot(t *testing.T) {
	tr := NewAgentTreeWithConcurrency("root", "m", 1)
	sc := tr.Scheduler()
	bb := NewBlackboard(tr)
	consumer := &AgentNode{ID: newNodeID(), Depth: 1}

	if err := sc.acquireSlot(context.Background(), ""); err != nil {
		t.Fatal("consumer acquire")
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := bb.Wait(ctx, "never", time.Minute, consumer) // yields the sole slot
		done <- err
	}()

	// After the yield, the slot is free — grab it from another party so the
	// consumer's later reacquire must queue.
	deadline := time.Now().Add(2 * time.Second)
	for sc.Snapshot().SlotsInUse != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if err := sc.acquireSlot(context.Background(), ""); err != nil {
		t.Fatal("other-party acquire of freed slot")
	}
	if got := sc.Snapshot().SlotsInUse; got != 1 {
		t.Fatalf("other party should hold the slot: SlotsInUse=%d, want 1", got)
	}

	cancel() // consumer's reacquire must queue behind the busy slot, then fail on cancel
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("Wait err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not return on cancel")
	}
	// The other party still holds the only slot; the consumer did NOT reclaim one.
	if got := sc.Snapshot().SlotsInUse; got != 1 {
		t.Fatalf("cancelled reacquire behind a busy slot must not reclaim: SlotsInUse=%d, want 1", got)
	}
	sc.releaseSlot() // release the other party's slot
}
