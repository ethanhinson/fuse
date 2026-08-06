package main

import (
	"fmt"

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
}

// pool converts the config pool into the agent-package mirror.
func (a workflowActivation) pool() agent.WorkflowPool {
	return agent.WorkflowPool{
		Concurrent: a.cfg.Pool.Concurrent,
		Total:      a.cfg.Pool.Total,
		MaxDepth:   a.cfg.Pool.MaxDepth,
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

// stripPredicateFor returns the strip predicate to install on a child at the
// given absolute depth. Outside a workflow (act == nil) it is the global-only
// predicate; inside, it composes the global predicate with the workflow's
// subtree-scoped one so the tighter brake governs.
func stripPredicateFor(tree *agent.AgentTree, maxConcurrent int, act *workflowActivation, rootID string, nodeDepth int) func() bool {
	global := agent.NewStripSpawnPredicate(tree, maxConcurrent)
	if act == nil || rootID == "" {
		return global
	}
	wf := agent.NewWorkflowStripPredicate(tree, rootID, act.pool(), nodeDepth, act.rootDepth)
	return agent.NewOrPredicate(global, wf)
}

// budgetFor returns the BudgetFunc to attach to a child's spawn tool. Inside a
// workflow with a total quota it reports the tighter of the workflow-total and
// the global budget; otherwise the plain global budget.
func budgetFor(tree *agent.AgentTree, act *workflowActivation, rootID string) tools.BudgetFunc {
	global := tools.BudgetFunc(tree.SpawnBudget)
	if act == nil || rootID == "" || act.cfg.Pool.Total <= 0 {
		return global
	}
	wfTotal := func() (used, max int) {
		return tree.SubtreeSpawnCount(rootID), act.cfg.Pool.Total
	}
	return tools.TighterBudget(global, wfTotal)
}
