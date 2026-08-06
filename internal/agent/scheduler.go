package agent

import (
	"context"
	"fmt"
	"sync"
)

// Scheduler is the single admission, slot, and per-pool policy authority for a
// spawn tree (change 0036, ADR-0007). It owns the global slot semaphore and the
// tree-global lifetime budget ceiling; the tree keeps only node data and counts.
// Every path that admits a spawn, takes or frees a slot, or reads the budget goes
// through a Scheduler method — nothing else touches the semaphore or the budget
// ceiling directly (spec Acceptance 1). The AgentTree constructs and owns exactly
// one Scheduler and exposes it via Scheduler(); the tree's own SetMaxSpawns /
// SpawnBudget / YieldSlot / UnyieldSlot methods survive as thin delegating shims.
//
// Task 1 is behavior-preserving: arrival-order wakeup on the semaphore is
// unchanged (the fair queue is Task 2), and Admit is a pure read describing the
// brakes 0033/0034 already enforce so the visibility predicate (Task 3) can be
// rebuilt on it without changing today's stripping semantics.
type Scheduler struct {
	tree *AgentTree

	// sem is the global width cap on concurrently RUNNING local children across
	// the whole tree. Queued children stay visible as pending nodes until a slot
	// frees. Sending on the channel acquires a slot; receiving frees one. The
	// blocked-sender wakeup order is Go's channel arrival order (FIFO in practice
	// for a full buffered channel); Task 2 replaces this with an explicit fair
	// grant queue.
	sem chan struct{}

	mu sync.Mutex
	// maxSpawns is the tree-global spawn budget ceiling — the total number of
	// child agents any spawn may create over the whole tree's life. 0 = unset (no
	// budget enforced). The "used" side of the budget is derived from the tree's
	// (append-only) node count, so only the ceiling lives here.
	maxSpawns int
	// pools holds per-workflow pool policy keyed by the workflow root's node ID,
	// registered when a workflow activates. Task 1 stores and returns policy but
	// does not yet dispatch on it; the fair queue (Task 2) and the unified
	// visibility predicate (Task 3) consume it. Guarded by mu.
	pools map[string]WorkflowPool
}

// newScheduler builds a Scheduler bound to tree, with a slot semaphore sized to
// maxConcurrent (the number of children that may run at once). A value <= 0
// falls back to MaxConcurrentSpawns, matching the pre-scheduler tree constructor.
func newScheduler(tree *AgentTree, maxConcurrent int) *Scheduler {
	if maxConcurrent <= 0 {
		maxConcurrent = MaxConcurrentSpawns
	}
	return &Scheduler{
		tree:  tree,
		sem:   make(chan struct{}, maxConcurrent),
		pools: map[string]WorkflowPool{},
	}
}

// SetMaxSpawns sets the tree-global spawn budget ceiling — the total number of
// child agents any spawn may create over the whole tree's life. 0 leaves it
// unset (no budget enforced). Set once at construction time, before any spawn.
func (s *Scheduler) SetMaxSpawns(n int) {
	s.mu.Lock()
	s.maxSpawns = n
	s.mu.Unlock()
}

// SpawnBudget reports how much of the tree-global spawn budget is used and its
// ceiling. `used` is the number of child agents created so far (every node
// except the root — the tree is append-only, so this only grows); `max` is the
// configured ceiling, 0 when unset. This is the count the runtime injects into
// each spawn_agent result so the model never has to tally its own spawns.
func (s *Scheduler) SpawnBudget() (used, max int) {
	used = s.tree.childCount()
	s.mu.Lock()
	max = s.maxSpawns
	s.mu.Unlock()
	return used, max
}

// RegisterPool records a workflow pool policy for the subtree rooted at rootID,
// to be consulted at admission and visibility time. Registered when a workflow
// activates on a node. A zero WorkflowPool (every dimension unset) is a no-op
// registration. Overwrites any prior policy for the same root.
func (s *Scheduler) RegisterPool(rootID string, pool WorkflowPool) {
	if rootID == "" {
		return
	}
	s.mu.Lock()
	s.pools[rootID] = pool
	s.mu.Unlock()
}

// Pool returns the workflow pool policy registered for rootID, and whether one
// was registered.
func (s *Scheduler) Pool(rootID string) (WorkflowPool, bool) {
	s.mu.Lock()
	p, ok := s.pools[rootID]
	s.mu.Unlock()
	return p, ok
}

// Verdict is the outcome of an admission decision.
type Verdict int

const (
	// Granted: a slot is available and the spawn may run now.
	Granted Verdict = iota
	// Queued: the spawn is admissible but the global slot cap is saturated, so it
	// waits. Task 1 waits in arrival order on the semaphore; Task 2 installs the
	// fair queue behind this verdict.
	Queued
	// Denied: a hard limit refuses the spawn — depth or lifetime budget. The
	// accompanying error preserves the existing error identity so the call-time
	// backstop in Spawn keeps returning the same messages.
	Denied
)

func (v Verdict) String() string {
	switch v {
	case Granted:
		return "granted"
	case Queued:
		return "queued"
	case Denied:
		return "denied"
	default:
		return "unknown"
	}
}

// AdmitRequest describes a would-be spawn for the admission decision. Depth is
// the spawning agent's depth (the child would be created at Depth+1), matching
// Spawner.depth. Task 2 extends this with the pool identity.
type AdmitRequest struct {
	Depth int
}

// Admit computes the current admission verdict for req without taking a slot or
// mutating any counter. It mirrors the brakes 0033/0034 already enforce so the
// decision is behavior-preserving:
//
//   - Denied (depth): a child at Depth+1 would exceed MaxDepth. Returns the
//     ErrMaxDepthExceeded identity.
//   - Denied (budget): the tree-global budget is exhausted (max > 0 && used >=
//     max). Returns the ErrSpawnBudgetExhausted identity with the same message
//     the Spawn backstop injects.
//   - Queued: the global active-child cap is saturated (running+pending at or
//     above the semaphore capacity) — the spawn would block for a slot.
//   - Granted: otherwise.
//
// The read is race-safe (it only calls tree methods that lock internally) and
// must be recomputed per call. It is the seam Task 2 (fair queue) and Task 3
// (unified visibility predicate) build on; the call-time refusal in Spawn stays
// as the race backstop regardless.
func (s *Scheduler) Admit(req AdmitRequest) (Verdict, error) {
	newDepth := req.Depth + 1
	if newDepth > MaxDepth {
		return Denied, fmt.Errorf("%w: depth %d > %d", ErrMaxDepthExceeded, newDepth, MaxDepth)
	}
	if used, max := s.SpawnBudget(); max > 0 && used >= max {
		return Denied, fmt.Errorf("%w: %d/%d spawns used — proceed with the results you already have and do not spawn again", ErrSpawnBudgetExhausted, used, max)
	}
	if cap(s.sem) > 0 {
		running, pending := s.tree.ActiveCounts()
		if running+pending >= cap(s.sem) {
			return Queued, nil
		}
	}
	return Granted, nil
}

// acquireSlot blocks until a global slot is free or ctx ends. Returns false when
// the context was cancelled first. Nil-safe: without a scheduler (or a sem) there
// is no cap.
func (s *Scheduler) acquireSlot(ctx context.Context) bool {
	if s == nil || s.sem == nil {
		return true
	}
	select {
	case s.sem <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

// releaseSlot frees a slot taken by acquireSlot.
func (s *Scheduler) releaseSlot() {
	if s == nil || s.sem == nil {
		return
	}
	<-s.sem
}

// YieldSlot releases node's slot while it blocks waiting on a child. Without
// this, width capping deadlocks: parents hold every slot while their children
// queue for one (observed live — 8 blocked parents, all children pending, zero
// progress; learning #12). A slot must mean "actively working", not "alive".
// Safe under parallel spawn batches: only the FIRST concurrent wait releases.
// No-op for the root (Depth 0 — it never holds a slot), nil nodes, and nil
// schedulers.
func (s *Scheduler) YieldSlot(node *AgentNode) {
	if s == nil || s.sem == nil || node == nil || node.Depth == 0 {
		return
	}
	node.mu.Lock()
	node.yields++
	first := node.yields == 1
	node.mu.Unlock()
	if first {
		s.releaseSlot()
	}
}

// UnyieldSlot re-acquires node's slot after a child wait completes; only the
// LAST of the node's concurrent waits re-acquires (blocking until a slot is free,
// which is the fair queueing point for resumed parents). Returns false if ctx
// ended first.
func (s *Scheduler) UnyieldSlot(ctx context.Context, node *AgentNode) bool {
	if s == nil || s.sem == nil || node == nil || node.Depth == 0 {
		return true
	}
	node.mu.Lock()
	node.yields--
	last := node.yields == 0
	node.mu.Unlock()
	if last {
		return s.acquireSlot(ctx)
	}
	return true
}
