package agent

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
)

// ErrQueueBoundExceeded is returned by a spawn whose pool's pending queue is
// already at its bound (ceil(queue_bound × pool slots), change 0036). The
// per-turn visibility predicate (Task 3) strips spawn_agent before the model gets
// this far; the error is the race backstop for a spawn that commits within a turn
// while the queue was under bound but has since filled — it refuses the spawn
// rather than growing the queue past the bound.
var ErrQueueBoundExceeded = errors.New("agent: spawn queue bound exceeded")

// defaultQueueBound is the multiplier applied to a pool's slot count to get its
// pending-queue ceiling (spec: pending ≤ 2× slots). Task 4 wires this to
// agents.queue_bound; until then it is a Scheduler field defaulting to this.
const defaultQueueBound = 2.0

// Scheduler is the single admission, slot, and per-pool policy authority for a
// spawn tree (change 0036, ADR-0007). It owns the global slot semaphore and the
// tree-global lifetime budget ceiling; the tree keeps only node data and counts.
// Every path that admits a spawn, takes or frees a slot, or reads the budget goes
// through a Scheduler method — nothing else touches the semaphore or the budget
// ceiling directly (spec Acceptance 1). The AgentTree constructs and owns exactly
// one Scheduler and exposes it via Scheduler(); the tree's own SetMaxSpawns /
// SpawnBudget / YieldSlot / UnyieldSlot methods survive as thin delegating shims.
//
// Task 2 replaces the arrival-order semaphore wakeup with an explicit fair grant
// dispatcher: spawn waiters land in their pool's FIFO, a freed slot is granted
// round-robin across non-empty pools (FIFO within a pool), per-pool pending is
// bounded, and unyield reacquisition jumps a priority lane. Admit is a pure read
// describing the brakes 0033/0034/this queue enforce so the visibility predicate
// (Task 3) can be rebuilt on it without changing today's stripping semantics.
type Scheduler struct {
	tree *AgentTree

	// slotCap is the global width cap on concurrently RUNNING local children
	// across the whole tree. Queued children stay visible as pending nodes until
	// a slot frees. Immutable after construction.
	slotCap int

	mu sync.Mutex
	// slotsInUse counts the slots currently granted (running children plus
	// reacquired-but-not-yet-running parents). Invariant: while any waiter is
	// queued, slotsInUse == slotCap — dispatch always drains free slots, so a
	// free slot means the queues are empty. Guarded by mu.
	slotsInUse int
	// priority is the reacquisition (unyield) lane: a resumed parent waiting for
	// its slot back never queues behind pool FIFOs, or it would deadlock behind
	// its own descendants' pending spawns (learning #12). FIFO. Guarded by mu.
	priority []*slotWaiter
	// poolQueues holds each pool's FIFO of spawn waiters, keyed by pool ID ("" is
	// the implicit session pool). poolOrder is the round-robin ring of pool IDs
	// with a non-empty queue, in first-seen order; rrCursor points at the pool to
	// serve next. Guarded by mu.
	poolQueues map[string][]*slotWaiter
	poolOrder  []string
	rrCursor   int
	// queueBound is the multiplier on a pool's slot count that caps its pending
	// queue (ceil(queueBound × slots)). Defaults to defaultQueueBound; Task 4
	// wires it to agents.queue_bound. Guarded by mu.
	queueBound float64
	// maxSpawns is the tree-global spawn budget ceiling — the total number of
	// child agents any spawn may create over the whole tree's life. 0 = unset (no
	// budget enforced). The "used" side of the budget is derived from the tree's
	// (append-only) node count, so only the ceiling lives here.
	maxSpawns int
	// sessionTokens is the global lifetime token ceiling for the whole session
	// (throughput.session_tokens, change 0036). 0 = unset (no ceiling enforced).
	// Measured against the whole-tree token sum (tree.SessionTokens, root
	// included). A permanent term: the per-node counters are append-only, so once
	// the session total reaches this ceiling spawn_agent stays stripped globally.
	// Guarded by mu.
	sessionTokens int
	// pools holds per-workflow pool policy keyed by the workflow root's node ID,
	// registered when a workflow activates. The fair queue (Task 2) reads a pool's
	// Concurrent figure to size its queue bound; the unified visibility predicate
	// (Task 3) consumes it too. Guarded by mu.
	pools map[string]WorkflowPool
}

// slotWaiter is one blocked acquirer. ready is signalled (a single send) by the
// dispatcher when the waiter is granted a slot; done guards the grant/cancel race
// so a waiter resolves exactly once.
type slotWaiter struct {
	pool  string
	ready chan struct{}
	done  bool
}

// newScheduler builds a Scheduler bound to tree, with a global slot cap of
// maxConcurrent (the number of children that may run at once). A value <= 0
// falls back to MaxConcurrentSpawns, matching the pre-scheduler tree constructor.
func newScheduler(tree *AgentTree, maxConcurrent int) *Scheduler {
	if maxConcurrent <= 0 {
		maxConcurrent = MaxConcurrentSpawns
	}
	return &Scheduler{
		tree:       tree,
		slotCap:    maxConcurrent,
		poolQueues: map[string][]*slotWaiter{},
		queueBound: defaultQueueBound,
		pools:      map[string]WorkflowPool{},
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

// SetQueueBound overrides the per-pool pending-queue multiplier (change 0036):
// a pool's FIFO holds at most ceil(mult × pool slots) waiters. A value <= 0 is a
// no-op that leaves the built-in default (defaultQueueBound) in place — this
// matches the config layer's zero-value = unset idiom, where an absent
// agents.queue_bound means "apply the default". Set once at construction time,
// before any spawn.
func (s *Scheduler) SetQueueBound(mult float64) {
	if mult <= 0 {
		return
	}
	s.mu.Lock()
	s.queueBound = mult
	s.mu.Unlock()
}

// SetSessionTokens sets the global session token ceiling — the total input+
// output tokens the whole session may spend before spawn_agent is stripped
// globally (throughput.session_tokens, change 0036). 0 leaves it unset (no
// ceiling). Set once at construction time, before any spawn; matches the config
// layer's zero-value = unset idiom, so an absent session_tokens is a no-op.
func (s *Scheduler) SetSessionTokens(n int) {
	s.mu.Lock()
	s.sessionTokens = n
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

// sessionTokenCeiling returns the configured global session token ceiling (0 =
// unset). Locks internally; safe under concurrent dispatch.
func (s *Scheduler) sessionTokenCeiling() int {
	s.mu.Lock()
	n := s.sessionTokens
	s.mu.Unlock()
	return n
}

// tokenQuotaDenied reports the token-quota refusal for a spawn originating at
// nodeID, or nil when no token quota is exhausted. It is the call-time backstop
// mirror of the token terms in Visible (change 0036): the session ceiling (a
// global whole-tree term) and the spawning node's workflow pool.tokens quota (a
// subtree term). The tighter session term is checked first. A non-nil result
// carries the ErrTokenQuotaExhausted identity, mirroring the budget backstop's
// discipline. Race-safe: it only calls tree/scheduler methods that lock.
func (s *Scheduler) tokenQuotaDenied(nodeID string) error {
	if s == nil {
		return nil
	}
	if ceil := s.sessionTokenCeiling(); ceil > 0 {
		if spent := s.tree.SessionTokens(); spent >= ceil {
			return fmt.Errorf("%w: session %d/%d tokens used — proceed with the results you already have and do not spawn again", ErrTokenQuotaExhausted, spent, ceil)
		}
	}
	if poolID := s.tree.WorkflowRootOf(nodeID); poolID != "" {
		if pool, ok := s.Pool(poolID); ok && pool.Tokens > 0 {
			if spent := s.tree.SubtreeTokens(poolID); spent >= pool.Tokens {
				return fmt.Errorf("%w: workflow %d/%d tokens used — proceed with the results you already have and do not spawn again", ErrTokenQuotaExhausted, spent, pool.Tokens)
			}
		}
	}
	return nil
}

// TokenQuotaWarning returns the machine-authored warning line to append to a
// spawn result originating at nodeID once a hard token quota is exhausted in its
// scope, or "" while every quota still has headroom (change 0036). It is the
// scope-aware source for the spawn tool's QuotaFunc: the session ceiling (a
// global term, so every node sees it once the whole-tree spend hits the ceiling)
// and the node's workflow pool.tokens quota (a subtree term, so only nodes
// inside the exhausted subtree see it). The returned string is pre-formatted
// (leading blank line, no trailing newline) to append directly, mirroring the
// budget line. Absent before exhaustion, absent outside the exhausted scope.
func (s *Scheduler) TokenQuotaWarning(nodeID string) string {
	if s == nil {
		return ""
	}
	if ceil := s.sessionTokenCeiling(); ceil > 0 && s.tree.SessionTokens() >= ceil {
		return "\n\ntoken quota exhausted: the session token ceiling is reached — conclude with the results you already have and do not spawn again"
	}
	if poolID := s.tree.WorkflowRootOf(nodeID); poolID != "" {
		if pool, ok := s.Pool(poolID); ok && pool.Tokens > 0 && s.tree.SubtreeTokens(poolID) >= pool.Tokens {
			return "\n\ntoken quota exhausted: this workflow's token quota is reached — conclude with the results you already have and do not spawn again"
		}
	}
	return ""
}

// Verdict is the outcome of an admission decision.
type Verdict int

const (
	// Granted: a slot is available and the spawn may run now.
	Granted Verdict = iota
	// Queued: the spawn is admissible but the global slot cap is saturated, so it
	// waits in its pool's FIFO, granted round-robin across pools as slots free.
	Queued
	// OverBound: the spawn's pool already has ceil(queue_bound × pool slots)
	// waiters pending, so admitting it would grow the queue past its bound. This
	// is the seam the visibility predicate (Task 3) strips on — the model stops
	// committing spawns it cannot have — and the call-time backstop in Spawn
	// refuses a racing spawn with ErrQueueBoundExceeded.
	OverBound
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
	case OverBound:
		return "over-bound"
	case Denied:
		return "denied"
	default:
		return "unknown"
	}
}

// AdmitRequest describes a would-be spawn for the admission decision. Depth is
// the spawning agent's depth (the child would be created at Depth+1), matching
// Spawner.depth. PoolID identifies the pool the spawn queues into — the workflow
// root's node ID (WorkflowRootOf of the spawning node), or "" for the implicit
// session pool shared by all freeform spawns.
type AdmitRequest struct {
	Depth  int
	PoolID string
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
//   - OverBound: the spawn's pool queue is already at ceil(queue_bound × pool
//     slots) waiters — admitting would grow it past its bound.
//   - Queued: the global active-child cap is saturated (running+pending at or
//     above the slot capacity) — the spawn would block for a slot, but the pool
//     still has queue headroom.
//   - Granted: otherwise.
//
// The read is race-safe (it only calls tree methods and scheduler methods that
// lock internally) and must be recomputed per call. It is the seam Task 3 (the
// unified visibility predicate) builds on; the call-time refusal in Spawn stays
// as the race backstop regardless.
func (s *Scheduler) Admit(req AdmitRequest) (Verdict, error) {
	newDepth := req.Depth + 1
	if newDepth > MaxDepth {
		return Denied, fmt.Errorf("%w: depth %d > %d", ErrMaxDepthExceeded, newDepth, MaxDepth)
	}
	if used, max := s.SpawnBudget(); max > 0 && used >= max {
		return Denied, fmt.Errorf("%w: %d/%d spawns used — proceed with the results you already have and do not spawn again", ErrSpawnBudgetExhausted, used, max)
	}
	if s.slotCap > 0 {
		// The spawn would queue when the global cap is saturated (tree running +
		// pending at or above capacity) OR the pool already has parked waiters —
		// either way there is no free slot to grant now. queuedDepth reflects
		// waiters currently blocked in the pool's FIFO, which is the authoritative
		// pending measure even before their pending nodes appear in the tree.
		running, pending := s.tree.ActiveCounts()
		depth := s.queuedDepth(req.PoolID)
		if running+pending >= s.slotCap || depth > 0 {
			if bound := s.poolQueueBound(req.PoolID); bound > 0 && depth >= bound {
				return OverBound, nil
			}
			return Queued, nil
		}
	}
	return Granted, nil
}

// Visible reports whether spawn_agent should be present in nodeID's tool schemas
// this turn: it is present iff an admission request from nodeID's scope would
// currently be granted or queued within bound (spec Acceptance 3, change 0036).
// This is the single rule every 0033/0034 strip variant collapses into:
//
//   - Global lifetime budget (permanent) and global slot cap (reversible, counts
//     running+PENDING) come from Admit: Denied ⇒ invisible; OverBound ⇒ invisible
//     (the pool's FIFO is at ceil(queue_bound × slots) — the NEW queue-bound
//     term); Granted or Queued ⇒ visible. Note the intended Task 3 shift: reaching
//     the global active-cap no longer strips — it queues (Queued is visible) — and
//     the strip moves to the queue bound, returning as the queue drains.
//   - Pool total (permanent), pool concurrent (reversible, subtree
//     running+PENDING) and pool max_depth (static) come from the workflow pool
//     registered for nodeID's nearest workflow root (WorkflowRootOf). These are
//     scoped to that subtree only — a sibling subtree with its own headroom stays
//     visible — and are unchanged from change 0034.
//
// The tighter of global and pool scope governs: any engaged term makes the node
// invisible. The read locks internally and MUST be recomputed every turn (never
// cached). A nil scheduler, or a nil/absent node, is always visible. The
// call-time backstops in Spawn remain the race safety net regardless.
func (s *Scheduler) Visible(nodeID string) bool {
	if s == nil {
		return true
	}
	node := s.tree.Node(nodeID)
	if node == nil {
		return true
	}
	poolID := s.tree.WorkflowRootOf(nodeID)

	// Global scope: budget, slot cap, and the pool's queue bound, via Admit.
	// Granted or Queued are the only visible verdicts (OverBound and Denied strip).
	v, _ := s.Admit(AdmitRequest{Depth: node.Depth, PoolID: poolID})
	if v != Granted && v != Queued {
		return false
	}

	// Global scope: session token ceiling (permanent, change 0036). When set, a
	// session whose whole-tree token sum has reached the ceiling strips
	// spawn_agent everywhere — the tightest global term alongside the budget.
	if ceil := s.sessionTokenCeiling(); ceil > 0 && s.tree.SessionTokens() >= ceil {
		return false
	}

	// Pool scope (change 0034): total/concurrent/max_depth, scoped to the subtree
	// rooted at poolID. Absent a registered pool (freeform spawn) none engage.
	if poolID != "" {
		if pool, ok := s.Pool(poolID); ok {
			root := s.tree.Node(poolID)
			rootDepth := 0
			if root != nil {
				rootDepth = root.Depth
			}
			// Static depth limit: at or beyond rootDepth+MaxDepth, never spawn.
			if pool.MaxDepth > 0 && node.Depth >= rootDepth+pool.MaxDepth {
				return false
			}
			// Total (permanent): the subtree's lifetime spawn quota.
			if pool.Total > 0 && s.tree.SubtreeSpawnCount(poolID) >= pool.Total {
				return false
			}
			// Tokens (permanent, change 0036): the subtree's lifetime token quota.
			// Same shape as Total — the subtree token sum is append-only, so once
			// it reaches the quota spawn_agent stays stripped in THIS subtree only;
			// a sibling workflow subtree with its own headroom is unaffected.
			if pool.Tokens > 0 && s.tree.SubtreeTokens(poolID) >= pool.Tokens {
				return false
			}
			// Concurrent (reversible): subtree running+pending at the pool cap.
			if pool.Concurrent > 0 {
				running, pending := s.tree.SubtreeActiveCounts(poolID)
				if running+pending >= pool.Concurrent {
					return false
				}
			}
		}
	}
	return true
}

// StripPredicate returns the per-turn spawn-strip predicate for nodeID: it strips
// (returns true) exactly when nodeID is not Visible. It is the unified replacement
// for the composed NewStripSpawnPredicate/NewWorkflowStripPredicate — one
// scheduler-backed rule for every scope. The predicate recomputes on each call
// (it MUST NOT be cached across turns) and is race-safe. A nil scheduler yields a
// predicate that never strips.
func (s *Scheduler) StripPredicate(nodeID string) func() bool {
	return func() bool {
		return !s.Visible(nodeID)
	}
}

// poolQueueBound reports the maximum number of waiters poolID's FIFO may hold:
// ceil(queueBound × pool slots). The pool's slot figure is its registered
// Concurrent cap when positive, else the global slot cap (which the implicit
// session pool always uses). A non-positive result means "no bound".
func (s *Scheduler) poolQueueBound(poolID string) int {
	s.mu.Lock()
	mult := s.queueBound
	slots := s.slotCap
	if poolID != "" {
		if p, ok := s.pools[poolID]; ok && p.Concurrent > 0 {
			slots = p.Concurrent
		}
	}
	s.mu.Unlock()
	if mult <= 0 || slots <= 0 {
		return 0
	}
	return int(math.Ceil(mult * float64(slots)))
}

// queuedDepth reports how many spawn waiters are currently parked in poolID's
// FIFO. The reacquisition priority lane is not a pool and is excluded. Test and
// admission accessor; safe under concurrent dispatch.
func (s *Scheduler) queuedDepth(poolID string) int {
	s.mu.Lock()
	n := len(s.poolQueues[poolID])
	s.mu.Unlock()
	return n
}

// priorityDepth reports how many reacquiring (unyield) parents are parked in the
// priority lane. Test accessor; safe under concurrent dispatch.
func (s *Scheduler) priorityDepth() int {
	s.mu.Lock()
	n := len(s.priority)
	s.mu.Unlock()
	return n
}

// acquireSlot blocks until a global slot is granted to a spawn in poolID or ctx
// ends. Waiters that cannot be granted immediately park in the pool's FIFO and
// are dispatched round-robin across pools as slots free (fair queue, change
// 0036). Returns false when the context was cancelled first; a cancelled waiter
// leaves the queue without leaking its entry or a slot. Nil-safe: without a
// scheduler (or a zero cap) there is no cap.
func (s *Scheduler) acquireSlot(ctx context.Context, poolID string) bool {
	return s.acquire(ctx, poolID, false)
}

// reacquireSlot re-takes a slot for a resumed parent (unyield) via the priority
// lane, jumping ahead of every pool FIFO so a parent never blocks behind its own
// descendants' pending spawns (the depth-2 deadlock, learning #12). Returns false
// on ctx cancellation.
func (s *Scheduler) reacquireSlot(ctx context.Context) bool {
	return s.acquire(ctx, "", true)
}

// acquire is the shared slot-acquisition path for both the pool FIFOs (priority
// == false) and the reacquisition lane (priority == true). Nil-safe.
func (s *Scheduler) acquire(ctx context.Context, poolID string, priority bool) bool {
	if s == nil || s.slotCap <= 0 {
		return true
	}
	s.mu.Lock()
	// Fast path: a free slot means no waiters are queued (dispatch always drains),
	// so granting here preserves FIFO order across the whole scheduler.
	if s.slotsInUse < s.slotCap && len(s.priority) == 0 && s.totalQueued() == 0 {
		s.slotsInUse++
		s.mu.Unlock()
		return true
	}
	w := &slotWaiter{pool: poolID, ready: make(chan struct{}, 1)}
	if priority {
		s.priority = append(s.priority, w)
	} else {
		s.enqueuePool(poolID, w)
	}
	s.mu.Unlock()

	select {
	case <-w.ready:
		return true
	case <-ctx.Done():
		s.mu.Lock()
		if w.done {
			// Granted concurrently with cancellation: honour the cancel by giving
			// the slot straight back to the next waiter rather than leaking it.
			s.mu.Unlock()
			s.releaseSlot()
			return false
		}
		w.done = true
		s.removeWaiter(w)
		s.mu.Unlock()
		return false
	}
}

// releaseSlot frees a slot taken by acquireSlot/reacquireSlot and dispatches it to
// the next waiter (priority lane first, then round-robin across pools).
func (s *Scheduler) releaseSlot() {
	if s == nil || s.slotCap <= 0 {
		return
	}
	s.mu.Lock()
	if s.slotsInUse > 0 {
		s.slotsInUse--
	}
	s.dispatch()
	s.mu.Unlock()
}

// dispatch grants free slots to waiting acquirers while any slot is free and any
// waiter is queued: the priority (reacquisition) lane drains first, then pool
// FIFOs round-robin. Caller holds mu.
func (s *Scheduler) dispatch() {
	for s.slotsInUse < s.slotCap {
		w := s.nextWaiter()
		if w == nil {
			return
		}
		w.done = true
		s.slotsInUse++
		w.ready <- struct{}{}
	}
}

// nextWaiter pops and returns the next waiter to grant, or nil when none are
// queued. Priority lane first (FIFO); then the pool queues round-robin from the
// rotating cursor, FIFO within the chosen pool. Caller holds mu.
func (s *Scheduler) nextWaiter() *slotWaiter {
	if len(s.priority) > 0 {
		w := s.priority[0]
		s.priority = s.priority[1:]
		if len(s.priority) == 0 {
			s.priority = nil
		}
		return w
	}
	for i := 0; i < len(s.poolOrder); i++ {
		idx := (s.rrCursor + i) % len(s.poolOrder)
		pool := s.poolOrder[idx]
		q := s.poolQueues[pool]
		if len(q) == 0 {
			continue
		}
		w := q[0]
		// Capture the successor pool by identity before mutating poolOrder: the
		// next grant should start there (round-robin interleave). dequeuePoolAt may
		// drop the served pool and shift poolOrder, so an index would go stale.
		successor := s.poolOrder[(idx+1)%len(s.poolOrder)]
		s.dequeuePoolAt(pool, 0)
		if len(s.poolOrder) > 0 {
			if pos := indexOf(s.poolOrder, successor); pos >= 0 {
				s.rrCursor = pos
			} else {
				// Successor was the served pool itself (a single-pool ring): reset.
				s.rrCursor = 0
			}
		}
		return w
	}
	return nil
}

// indexOf returns the position of v in xs, or -1. Caller holds mu.
func indexOf(xs []string, v string) int {
	for i, x := range xs {
		if x == v {
			return i
		}
	}
	return -1
}

// enqueuePool appends w to poolID's FIFO, registering the pool in the round-robin
// ring on its first waiter. Caller holds mu.
func (s *Scheduler) enqueuePool(poolID string, w *slotWaiter) {
	if len(s.poolQueues[poolID]) == 0 {
		s.poolOrder = append(s.poolOrder, poolID)
	}
	s.poolQueues[poolID] = append(s.poolQueues[poolID], w)
}

// dequeuePoolAt removes the waiter at index i from poolID's FIFO, dropping the
// pool from the round-robin ring when its queue empties. Caller holds mu.
func (s *Scheduler) dequeuePoolAt(poolID string, i int) {
	q := s.poolQueues[poolID]
	q = append(q[:i], q[i+1:]...)
	if len(q) == 0 {
		delete(s.poolQueues, poolID)
		s.removePoolFromOrder(poolID)
	} else {
		s.poolQueues[poolID] = q
	}
}

// removePoolFromOrder drops poolID from poolOrder and fixes the cursor so it keeps
// pointing at the same logical position. Caller holds mu.
func (s *Scheduler) removePoolFromOrder(poolID string) {
	for i, p := range s.poolOrder {
		if p != poolID {
			continue
		}
		s.poolOrder = append(s.poolOrder[:i], s.poolOrder[i+1:]...)
		if i < s.rrCursor {
			s.rrCursor--
		}
		break
	}
	if len(s.poolOrder) == 0 {
		s.rrCursor = 0
	} else {
		s.rrCursor %= len(s.poolOrder)
	}
}

// removeWaiter pulls w out of whichever queue holds it (priority lane or a pool
// FIFO) on cancellation. Caller holds mu.
func (s *Scheduler) removeWaiter(w *slotWaiter) {
	for i, pw := range s.priority {
		if pw == w {
			s.priority = append(s.priority[:i], s.priority[i+1:]...)
			if len(s.priority) == 0 {
				s.priority = nil
			}
			return
		}
	}
	for _, q := range s.poolQueues {
		for i, pw := range q {
			if pw == w {
				s.dequeuePoolAt(w.pool, i)
				return
			}
		}
	}
}

// totalQueued reports the number of parked pool-FIFO waiters (priority lane
// excluded). Caller holds mu.
func (s *Scheduler) totalQueued() int {
	n := 0
	for _, q := range s.poolQueues {
		n += len(q)
	}
	return n
}

// YieldSlot releases node's slot while it blocks waiting on a child. Without
// this, width capping deadlocks: parents hold every slot while their children
// queue for one (observed live — 8 blocked parents, all children pending, zero
// progress; learning #12). A slot must mean "actively working", not "alive".
// Safe under parallel spawn batches: only the FIRST concurrent wait releases.
// No-op for the root (Depth 0 — it never holds a slot), nil nodes, and nil
// schedulers.
func (s *Scheduler) YieldSlot(node *AgentNode) {
	if s == nil || s.slotCap <= 0 || node == nil || node.Depth == 0 {
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
// LAST of the node's concurrent waits re-acquires. Reacquisition goes through the
// priority lane (reacquireSlot), never a pool FIFO: a resumed parent must jump
// ahead of pending spawns — including its own descendants' — or the depth-2
// deadlock returns (learning #12). Returns false if ctx ended first.
func (s *Scheduler) UnyieldSlot(ctx context.Context, node *AgentNode) bool {
	if s == nil || s.slotCap <= 0 || node == nil || node.Depth == 0 {
		return true
	}
	node.mu.Lock()
	node.yields--
	last := node.yields == 0
	node.mu.Unlock()
	if last {
		return s.reacquireSlot(ctx)
	}
	return true
}
