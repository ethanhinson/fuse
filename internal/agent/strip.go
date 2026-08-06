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
//     reappears on the next turn.
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
