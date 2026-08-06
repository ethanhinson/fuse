package agent

// NewStripSpawnPredicate returns a per-turn predicate deciding whether the
// spawn_agent tool must be omitted from an agent's tool schemas this turn.
//
// It strips when either brake is engaged:
//
//   - Lifetime budget (permanent): the tree-global spawn budget is exhausted,
//     i.e. max > 0 && used >= max. Once true it stays true for the session,
//     because the tree is append-only.
//   - Active-child cap (reversible): the tree's active child count
//     (running + pending) is at or above maxConcurrent. As children finish and
//     the count drops below the cap, this term becomes false again and the tool
//     reappears on the next turn. Note the asymmetry with the runtime semaphore:
//     the semaphore bounds only RUNNING children, whereas this strip term counts
//     running + PENDING. That is deliberate — stripping on the queued total keeps
//     the model from stacking more spawns behind a saturated semaphore (covered
//     by TestStripPredicatePendingCountsTowardCap).
//
// The predicate reads only AgentTree methods that lock internally, so it is
// race-safe, and it recomputes on every call — it MUST NOT be cached across
// turns. A nil tree yields a predicate that never strips.
func NewStripSpawnPredicate(tree *AgentTree, maxConcurrent int) func() bool {
	return func() bool {
		if tree == nil {
			return false
		}
		if used, max := tree.SpawnBudget(); max > 0 && used >= max {
			return true
		}
		if maxConcurrent > 0 {
			running, pending := tree.ActiveCounts()
			if running+pending >= maxConcurrent {
				return true
			}
		}
		return false
	}
}

// WorkflowPool is the agent-package mirror of a workflow's pool policy, kept
// here so internal/agent never imports internal/config (the config layer builds
// this from its PoolConfig). Each dimension is 0 = unset (that brake off).
type WorkflowPool struct {
	Concurrent int // reversible: max running+pending children in the subtree
	Total      int // permanent: lifetime spawn quota for the subtree
	MaxDepth   int // static: spawn depth below the workflow root
}

// NewWorkflowStripPredicate returns a per-turn predicate that strips spawn_agent
// for a node INSIDE a workflow subtree, applying the workflow's pool at subtree
// scope (change 0034). It mirrors NewStripSpawnPredicate's reversible/permanent
// asymmetry, but every count is scoped to the subtree rooted at rootID:
//
//   - Total (permanent): SubtreeSpawnCount(rootID) >= pool.Total. Once true it
//     stays true (the tree is append-only).
//   - Concurrent (reversible): subtree running+pending >= pool.Concurrent;
//     reappears as subtree children exit. Counting pending mirrors the global
//     predicate — it keeps the model from stacking spawns behind a saturated
//     pool.
//   - MaxDepth (static): a node at rootDepth+pool.MaxDepth is at the workflow's
//     depth limit and can never spawn, independent of subtree counts.
//
// nodeDepth is the absolute tree depth of the node this predicate is installed
// on; rootDepth is the workflow root's absolute depth. A zero dimension disables
// that brake, matching SpawnBudget's max==0 and the cap's <=0 conventions. A nil
// tree or empty rootID never strips.
//
// This predicate does NOT subsume the global one — it is composed with it via
// orPredicates so the TIGHTER of workflow-scope and global-scope governs, and
// the call-time backstops in Spawn remain the race safety net either way.
func NewWorkflowStripPredicate(tree *AgentTree, rootID string, pool WorkflowPool, nodeDepth, rootDepth int) func() bool {
	return func() bool {
		if tree == nil || rootID == "" {
			return false
		}
		// Static depth limit: at or beyond rootDepth+MaxDepth, never spawn.
		if pool.MaxDepth > 0 && nodeDepth >= rootDepth+pool.MaxDepth {
			return true
		}
		if pool.Total > 0 && tree.SubtreeSpawnCount(rootID) >= pool.Total {
			return true
		}
		if pool.Concurrent > 0 {
			running, pending := tree.SubtreeActiveCounts(rootID)
			if running+pending >= pool.Concurrent {
				return true
			}
		}
		return false
	}
}

// orPredicates composes strip predicates: the result strips when ANY operand
// strips (the tighter brake wins). Nil operands are ignored. A nil/empty set
// never strips.
func orPredicates(preds ...func() bool) func() bool {
	return func() bool {
		for _, p := range preds {
			if p != nil && p() {
				return true
			}
		}
		return false
	}
}
