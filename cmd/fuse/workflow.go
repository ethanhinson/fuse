package main

import (
	"fmt"
	"sync/atomic"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/tools"
)

// workflowActivation carries the resolved policy for a workflow root's subtree.
// It is assembled once when a workflow-bound skill activates and threaded into
// the child builder so each child is registry- and strip-scoped to the pool.
type workflowActivation struct {
	name string
	cfg  config.WorkflowConfig
	// rootDepth is the absolute tree depth of the workflow root node; the pool's
	// max_depth is measured relative to it.
	rootDepth int
	// reserved is the atomic count of spawns admitted by the backstop. It is the
	// race-free counter behind the total quota: because 2+ spawn_agent calls in a
	// single turn run CONCURRENTLY (agent loop), a snapshot of the tree is racy —
	// each concurrent call could read a stale sub-quota count and all be admitted.
	// An atomic reserve-then-check closes that window so a within-turn batch can
	// never overshoot pool.Total. Shared across every spawner in the run (the
	// activation is created once at root activation).
	//
	// Two counters track the same permanent `total` quota, and they use
	// deliberately different comparisons against pool.Total:
	//   - `reserved` (backstop, here) is the authoritative gate. It reserves a
	//     slot BEFORE spawnLocal, so it counts the in-flight reservation and
	//     rejects on `reserved > Total` (strictly greater — the Nth spawn is
	//     admitted, the N+1th is not).
	//   - tree.SubtreeSpawnCount(rootID) (the strip predicate and budgetFor,
	//     below) counts COMMITTED nodes that addNode has already recorded, so it
	//     strips on `>= Total`.
	// The two count the same quota from opposite sides of the same spawnLocal
	// call and so track closely; the operator difference (`>` vs `>=`) reflects
	// only in-flight-vs-committed, not divergent policy. `reserved` is
	// "permanent": it is decremented only on its own over-quota rejection, so a
	// spawn cancelled downstream still consumes a total slot (acceptable for v1).
	reserved atomic.Int64
}

// pool converts the config pool into the agent-package mirror.
func (a workflowActivation) pool() agent.WorkflowPool {
	return agent.WorkflowPool{
		Concurrent: a.cfg.Pool.Concurrent,
		Total:      a.cfg.Pool.Total,
		MaxDepth:   a.cfg.Pool.MaxDepth,
		Tokens:     a.cfg.Pool.Tokens,
	}
}

// workerNames returns the workflow's worker names (for the spawn tool's enum),
// or nil when the workflow defines no workers (freeform spawns).
func (a workflowActivation) workerNames() []string {
	if len(a.cfg.Workers) == 0 {
		return nil
	}
	names := make([]string, 0, len(a.cfg.Workers))
	for n := range a.cfg.Workers {
		names = append(names, n)
	}
	return names
}

// resolveWorkerTools returns the effective tool allowlist for a spawn inside the
// workflow, given the model-selected worker name and any narrowing tools subset.
// Rules (change 0034):
//   - worker == "": no worker selected; the requested subset (possibly empty =
//     inherit) governs, unchanged from freeform behavior.
//   - worker names an unknown worker: an error (the model self-corrects).
//   - worker names a known worker: the child's tools are that worker's allowlist,
//     and a non-empty requested subset may only NARROW it (any requested tool not
//     in the allowlist is rejected). The allowlist is authoritative — a worker
//     without spawn_agent cannot regain it via the subset.
func resolveWorkerTools(wf config.WorkflowConfig, worker string, requested []string) ([]string, error) {
	if worker == "" {
		return requested, nil
	}
	w, ok := wf.Workers[worker]
	if !ok {
		return nil, fmt.Errorf("unknown worker %q", worker)
	}
	allow := map[string]bool{}
	for _, t := range w.Tools {
		allow[t] = true
	}
	if len(requested) == 0 {
		return append([]string(nil), w.Tools...), nil
	}
	// Narrow: every requested tool must be in the allowlist.
	var out []string
	for _, r := range requested {
		if !allow[r] {
			return nil, fmt.Errorf("worker %q does not permit tool %q", worker, r)
		}
		out = append(out, r)
	}
	return out, nil
}

// backstopFor returns the per-call workflow spawn backstop for a spawner rooted
// under the workflow, or nil outside a workflow. It refuses a spawn that would
// exceed the pool's total quota or push a child past the pool's max_depth — the
// race/batch safety net behind the per-turn schema strip, mirroring the global
// budget/depth backstops (change 0034 Acceptance 1: the limits hold regardless
// of what the model attempts within a single turn).
func backstopFor(tree *agent.AgentTree, act *workflowActivation, rootID string) func(newDepth int) error {
	if act == nil || rootID == "" {
		return nil
	}
	pool := act.pool()
	if pool.Total <= 0 && pool.MaxDepth <= 0 {
		return nil
	}
	return func(newDepth int) error {
		if pool.MaxDepth > 0 && newDepth > act.rootDepth+pool.MaxDepth {
			return fmt.Errorf("%w: depth %d exceeds workflow %q max_depth %d",
				agent.ErrWorkflowQuotaExhausted, newDepth, act.name, pool.MaxDepth)
		}
		if pool.Total > 0 {
			// Atomically reserve a slot; if this reservation would exceed the
			// quota, release it and refuse. Reserve-then-check (not a tree
			// snapshot) is what makes the cap hold under a concurrent batch.
			if n := act.reserved.Add(1); n > int64(pool.Total) {
				act.reserved.Add(-1)
				return fmt.Errorf("%w: %d/%d workflow %q spawns used — proceed with the results you already have",
					agent.ErrWorkflowQuotaExhausted, pool.Total, pool.Total, act.name)
			}
		}
		return nil
	}
}

// budgetFor returns the BudgetFunc to attach to a child's spawn tool. Inside a
// workflow with a total quota it reports the tighter of the workflow-total and
// the global budget; otherwise the plain global budget.
func budgetFor(tree *agent.AgentTree, act *workflowActivation, rootID string) tools.BudgetFunc {
	global := tools.BudgetFunc(tree.Scheduler().SpawnBudget)
	if act == nil || rootID == "" || act.cfg.Pool.Total <= 0 {
		return global
	}
	wfTotal := func() (used, max int) {
		return tree.SubtreeSpawnCount(rootID), act.cfg.Pool.Total
	}
	return tools.TighterBudget(global, wfTotal)
}

// quotaWarningFor returns the token-quota warning QuotaFunc to attach to a
// child's spawn tool (change 0036). It is scope-aware via the scheduler: the
// warning line is empty until a hard token quota is exhausted for nodeID's scope
// — the global session ceiling (throughput.session_tokens) or the node's
// workflow pool.tokens quota — after which it is appended to subsequent spawn
// results so the agent concludes with what it has. Read fresh at result time.
func quotaWarningFor(tree *agent.AgentTree, nodeID string) tools.QuotaFunc {
	return func() string {
		return tree.Scheduler().TokenQuotaWarning(nodeID)
	}
}
