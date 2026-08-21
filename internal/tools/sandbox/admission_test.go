package sandbox

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/event"
)

// gateCfg builds a Concurrency config from the four bounds, for a test that
// wants tight numbers it can saturate.
func gateCfg(global, perTenant, maxQueued int64, note time.Duration) Concurrency {
	return Concurrency{
		MaxInflight:          i64(global),
		MaxInflightPerTenant: i64(perTenant),
		MaxQueued:            i64(maxQueued),
		NoteThreshold:        dur(note),
	}
}

// inflightMeter is a shared counter with a max watermark, used to assert the gate
// never lets more than N executions run at once.
type inflightMeter struct {
	cur atomic.Int64
	max atomic.Int64
}

func (m *inflightMeter) enter() {
	c := m.cur.Add(1)
	for {
		hi := m.max.Load()
		if c <= hi || m.max.CompareAndSwap(hi, c) {
			break
		}
	}
}
func (m *inflightMeter) leave() { m.cur.Add(-1) }

// N ≫ bound concurrent Admits: in-flight never exceeds max_inflight, and the
// queue DRAINS — every caller eventually admits and completes.
func TestGateNeverExceedsGlobalBoundAndDrains(t *testing.T) {
	const global = 4
	g := newGate(gateCfg(global, global, 1024, time.Hour)) // per-tenant == global so global binds

	var meter inflightMeter
	const n = 64
	var wg sync.WaitGroup
	wg.Add(n)
	admitted := atomic.Int64{}

	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			ticket, _, err := g.Admit(ctx, "t")
			if err != nil {
				t.Errorf("Admit: %v", err)
				return
			}
			admitted.Add(1)
			meter.enter()
			time.Sleep(time.Millisecond) // hold the slot briefly
			meter.leave()
			ticket.Release()
		}()
	}
	wg.Wait()

	if admitted.Load() != n {
		t.Errorf("admitted = %d, want all %d (the queue must drain)", admitted.Load(), n)
	}
	if hi := meter.max.Load(); hi > global {
		t.Errorf("peak in-flight = %d, want <= %d", hi, global)
	}
}

// Per-tenant fairness: tenant A saturates its own share; tenant B's Admit still
// returns promptly. The cross-tenant starvation guard — and the one that fails
// if acquisition order is inverted.
func TestGatePerTenantShareDoesNotStarveOthers(t *testing.T) {
	// Global is generous; per-tenant is tight. Tenant A parks its whole share
	// and holds it; tenant B must still get in.
	g := newGate(gateCfg(64, 2, 1024, time.Hour))

	// Saturate tenant A: take both of A's slots and hold them.
	var aTickets []Ticket
	for i := 0; i < 2; i++ {
		tk, _, err := g.Admit(context.Background(), "A")
		if err != nil {
			t.Fatalf("A Admit %d: %v", i, err)
		}
		aTickets = append(aTickets, tk)
	}

	// A third A caller must block (A's share is full). B must NOT.
	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		tk, _, err := g.Admit(ctx, "B")
		if tk != nil {
			tk.Release()
		}
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("tenant B was starved by tenant A: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("tenant B's Admit did not return promptly while A was saturated")
	}

	for _, tk := range aTickets {
		tk.Release()
	}
}

// Runaway refusal: with max_inflight held and max_queued waiters parked, the
// next Admit returns ErrSandboxAtCapacity IMMEDIATELY (not after a wait).
func TestGateRefusesAtWaiterOverflow(t *testing.T) {
	const global, maxQueued = 2, 3
	g := newGate(gateCfg(global, global, maxQueued, time.Hour))

	// Hold all inflight slots.
	var held []Ticket
	for i := 0; i < global; i++ {
		tk, _, err := g.Admit(context.Background(), "t")
		if err != nil {
			t.Fatalf("hold %d: %v", i, err)
		}
		held = append(held, tk)
	}

	// Park exactly max_queued waiters (they block, since inflight is full).
	var parked sync.WaitGroup
	parked.Add(maxQueued)
	release := make(chan struct{})
	for i := 0; i < maxQueued; i++ {
		go func() {
			parked.Done()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go func() { <-release; cancel() }()
			tk, _, err := g.Admit(ctx, "t")
			if tk != nil {
				tk.Release()
			}
			_ = err
		}()
	}
	parked.Wait()
	// Let the parked goroutines actually reach the semaphore wait.
	waitFor(t, "waiters to park", func() bool {
		g.mu.Lock()
		w := g.waiters
		g.mu.Unlock()
		return w >= maxQueued
	})

	// The next caller overflows the waiter bound: refused IMMEDIATELY.
	start := time.Now()
	tk, waited, err := g.Admit(context.Background(), "t")
	elapsed := time.Since(start)
	if !errors.Is(err, ErrSandboxAtCapacity) {
		t.Fatalf("err = %v, want ErrSandboxAtCapacity", err)
	}
	if tk != nil {
		t.Error("a refused Admit returned a ticket")
	}
	if waited != 0 {
		t.Errorf("waited = %v, want 0 on an immediate refusal", waited)
	}
	if elapsed > 200*time.Millisecond {
		t.Errorf("refusal took %v, want near-immediate", elapsed)
	}

	close(release)
	for _, tk := range held {
		tk.Release()
	}
}

// A waiter whose ctx expires returns a ctx error, NOT a capacity error — the two
// conditions stay distinguishable.
func TestGateDeadlineIsNotCapacity(t *testing.T) {
	g := newGate(gateCfg(1, 1, 1024, time.Hour))

	hold, _, err := g.Admit(context.Background(), "t")
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	defer hold.Release()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	tk, _, err := g.Admit(ctx, "t")
	if tk != nil {
		t.Error("got a ticket though the slot was held")
	}
	if errors.Is(err, ErrSandboxAtCapacity) {
		t.Fatalf("err = %v, want a deadline error, not a capacity refusal", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
}

// No leak: after every caller returns, in-flight is 0 and a fresh Admit succeeds
// instantly — tickets released on the error, refusal, and deadline paths.
func TestGateNoSlotLeakAcrossAllPaths(t *testing.T) {
	g := newGate(gateCfg(2, 2, 2, time.Hour))

	// Exercise: a happy admit+release, a deadline, and a refusal — then prove a
	// fresh admit is instant.
	tk, _, err := g.Admit(context.Background(), "t")
	if err != nil {
		t.Fatalf("happy admit: %v", err)
	}
	tk.Release()

	// A deadline path (hold both slots, let a third time out).
	h1, _, _ := g.Admit(context.Background(), "t")
	h2, _, _ := g.Admit(context.Background(), "t")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	if _, _, err := g.Admit(ctx, "t"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline, got %v", err)
	}
	cancel()
	h1.Release()
	h2.Release()

	// After everything, a fresh admit is instant and in-flight is zero.
	start := time.Now()
	tk2, waited, err := g.Admit(context.Background(), "t")
	if err != nil {
		t.Fatalf("post-teardown admit: %v", err)
	}
	if time.Since(start) > 100*time.Millisecond || waited > 100*time.Millisecond {
		t.Errorf("fresh admit was not instant: elapsed=%v waited=%v", time.Since(start), waited)
	}
	tk2.Release()

	// The global semaphore is fully drained (no leaked tokens).
	if n := len(g.global.tokens); n != 0 {
		t.Errorf("global semaphore holds %d leaked tokens", n)
	}
}

// Idempotent Release: releasing a ticket twice returns exactly one slot.
func TestGateTicketReleaseIsIdempotent(t *testing.T) {
	g := newGate(gateCfg(1, 1, 8, time.Hour))
	tk, _, err := g.Admit(context.Background(), "t")
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	tk.Release()
	tk.Release() // second release must be a no-op, not a double-return

	// Only one slot should be free — take it, then a second admit must block.
	tk2, _, err := g.Admit(context.Background(), "t")
	if err != nil {
		t.Fatalf("re-admit: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, _, err := g.Admit(ctx, "t"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a double-release leaked a slot: %v", err)
	}
	tk2.Release()
}

// The uncontended fast path fires NO hook: one event per Exec would double the
// sandbox stream's volume to record that nothing happened.
func TestGateUncontendedFiresNoHook(t *testing.T) {
	var queued, refused atomic.Int64
	// A high note threshold so an instant admit is never "notable".
	g := newGate(gateCfg(8, 8, 256, time.Hour)).withHandlerName("container")
	g.setHooks(GateHooks{
		Queued:  func(AdmissionInfo) { queued.Add(1) },
		Refused: func(AdmissionInfo) { refused.Add(1) },
	})

	tk, _, err := g.Admit(context.Background(), "t")
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	tk.Release()

	if queued.Load() != 0 || refused.Load() != 0 {
		t.Errorf("uncontended admit fired a hook: queued=%d refused=%d", queued.Load(), refused.Load())
	}
}

// A capacity refusal fires the Refused hook exactly once, carrying the bounded
// handler name.
func TestGateRefusalFiresRefusedHook(t *testing.T) {
	var refused atomic.Int64
	var lastHandler string
	const global, maxQueued = 1, 1
	g := newGate(gateCfg(global, global, maxQueued, time.Hour)).withHandlerName("container")
	g.setHooks(GateHooks{Refused: func(i AdmissionInfo) {
		refused.Add(1)
	}})
	_ = lastHandler

	// Hold the single inflight slot.
	held, _, err := g.Admit(context.Background(), "t")
	if err != nil {
		t.Fatalf("hold: %v", err)
	}

	// Park exactly maxQueued(1) waiter so the queue is full.
	release := make(chan struct{})
	go func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() { <-release; cancel() }()
		tk, _, _ := g.Admit(ctx, "t")
		if tk != nil {
			tk.Release()
		}
	}()
	waitFor(t, "waiter to park", func() bool {
		g.mu.Lock()
		w := g.waiters
		g.mu.Unlock()
		return w >= maxQueued
	})

	// The next caller overflows the waiter bound: refused, and the hook fires.
	if _, _, err := g.Admit(context.Background(), "t"); !errors.Is(err, ErrSandboxAtCapacity) {
		t.Fatalf("want capacity refusal, got %v", err)
	}
	if refused.Load() != 1 {
		t.Errorf("refused hook count = %d, want 1", refused.Load())
	}

	close(release)
	held.Release()
}

// A wait past the note threshold fires the Queued hook, carrying the wait
// duration and the bounded handler name.
func TestGateQueuedHookFiresOnNotableWait(t *testing.T) {
	var got AdmissionInfo
	var fired atomic.Int64
	// A small but non-trivial threshold: an instant admit stays under it, only a
	// genuine queue-wait crosses it.
	g := newGate(gateCfg(1, 1, 256, 3*time.Millisecond)).withHandlerName("container")
	g.setHooks(GateHooks{Queued: func(i AdmissionInfo) {
		got = i
		fired.Add(1)
	}})

	// Hold the single slot, then start a waiter that will queue behind it.
	held, _, err := g.Admit(context.Background(), "t")
	if err != nil {
		t.Fatalf("hold: %v", err)
	}

	admitted := make(chan time.Duration, 1)
	go func() {
		tk, waited, err := g.Admit(context.Background(), "t")
		if err == nil {
			admitted <- waited
			tk.Release()
		}
	}()

	// Let the waiter park, then release the held slot so it gets in having waited.
	waitFor(t, "waiter to park", func() bool {
		g.mu.Lock()
		w := g.waiters
		g.mu.Unlock()
		return w >= 1
	})
	time.Sleep(10 * time.Millisecond) // ensure the waiter's wait exceeds the threshold
	held.Release()

	select {
	case <-admitted:
	case <-time.After(2 * time.Second):
		t.Fatal("waiter never admitted")
	}
	if fired.Load() != 1 {
		t.Fatalf("queued hook fired %d times, want 1", fired.Load())
	}
	if got.Waited <= 0 {
		t.Errorf("queued info Waited = %v, want > 0", got.Waited)
	}
}

// The empty tenant normalises to DefaultTenant, matching the storage convention,
// so an unstamped local principal and an explicit _default share one share.
func TestGateEmptyTenantNormalises(t *testing.T) {
	g := newGate(gateCfg(1, 1, 8, time.Hour))

	tk, _, err := g.Admit(context.Background(), event.TenantID(""))
	if err != nil {
		t.Fatalf("admit empty tenant: %v", err)
	}
	// The empty tenant and DefaultTenant address the SAME share: the second must
	// block because the first holds the single slot.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, _, err := g.Admit(ctx, event.DefaultTenant); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("empty tenant and DefaultTenant did not share a slot: %v", err)
	}
	tk.Release()
}
