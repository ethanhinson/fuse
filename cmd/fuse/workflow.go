package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
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
func (a *workflowActivation) pool() agent.WorkflowPool {
	return agent.WorkflowPool{
		Concurrent: a.cfg.Pool.Concurrent,
		Total:      a.cfg.Pool.Total,
		MaxDepth:   a.cfg.Pool.MaxDepth,
		Tokens:     a.cfg.Pool.Tokens,
	}
}

// workerNames returns the workflow's worker names, sorted for a stable schema
// (for the spawn tool's enum), or nil when the workflow defines no workers
// (freeform spawns).
func (a *workflowActivation) workerNames() []string {
	return sortedWorkerNames(a.cfg)
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

// workflowScope is one loop's workflow activation state (change 0091). Every
// binding builds one per agent tree; activations are keyed by the workflow-root
// node id, so two roots of the same workflow each get their own pool and quota.
//
// The activation (and so its atomic `reserved` quota counter) is created at most
// once per root, under mu, and every later lookup for any node in that subtree
// returns the same pointer. That single-instance rule is what lets the backstop
// hold pool.total across every spawner the subtree builds.
type workflowScope struct {
	tree      *agent.AgentTree
	workflows map[string]config.WorkflowConfig
	mu        sync.Mutex
	active    map[string]*workflowActivation
}

// newWorkflowScope returns the activation scope for tree over the configured
// workflows (nil or empty = no workflow can activate).
func newWorkflowScope(tree *agent.AgentTree, workflows map[string]config.WorkflowConfig) *workflowScope {
	return &workflowScope{
		tree:      tree,
		workflows: workflows,
		active:    map[string]*workflowActivation{},
	}
}

// childPlan is the workflow-resolved shape of one child spawn.
type childPlan struct {
	tools []string // effective tool list (nil = inherit all)
	model string   // worker model pin, "" when none
	bound bool     // a worker was selected inside a workflow
}

// names returns the configured workflow names, sorted.
func (s *workflowScope) names() []string {
	return sortedWorkflowNames(s.workflows)
}

// check refuses a workflow name that is not configured, listing the ones that
// are so the model can self-correct.
func (s *workflowScope) check(name string) error {
	if _, ok := s.workflows[name]; ok {
		return nil
	}
	return unknownWorkflowErr(name, s.names())
}

// unknownWorkflowErr is the one refusal text for a workflow name that is not
// configured, shared by workflowScope.check and the pipeline synthesis seam.
// names is the sorted list of configured workflows.
func unknownWorkflowErr(name string, names []string) error {
	list := "none configured"
	if len(names) > 0 {
		list = strings.Join(names, ", ")
	}
	return fmt.Errorf("unknown workflow %q (available: %s)", name, list)
}

// activateWorkflow activates the workflow node roots (its WorkflowRoot marker):
// it registers the pool with the scheduler and records the activation. It is
// idempotent per node id, so concurrent or repeated calls share one activation
// and one quota counter.
func (s *workflowScope) activateWorkflow(node *agent.AgentNode) (*workflowActivation, error) {
	if node == nil {
		return nil, fmt.Errorf("activate workflow: no node")
	}
	name := node.Snapshot().WorkflowRoot
	if name == "" {
		return nil, fmt.Errorf("activate workflow: node %s is not a workflow root", node.ID)
	}
	if err := s.check(name); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if act, ok := s.active[node.ID]; ok {
		return act, nil
	}
	act := &workflowActivation{name: name, cfg: s.workflows[name], rootDepth: node.Depth}
	// Registered under mu so no lookup can observe the activation before its pool.
	s.tree.Scheduler().RegisterPool(node.ID, act.pool())
	s.active[node.ID] = act
	return act, nil
}

// activateRoot is the one root-level activation (the research probe): it marks
// node as the root of name and activates it. It must run before node runs or is
// visible to any reader, matching AgentNode.WorkflowRoot's create-once invariant
// (the field is written here without the node lock).
func (s *workflowScope) activateRoot(node *agent.AgentNode, name string) (*workflowActivation, error) {
	if err := s.check(name); err != nil {
		return nil, err
	}
	node.WorkflowRoot = name
	return s.activateWorkflow(node)
}

// activationOf returns the activation governing nodeID and its workflow-root id,
// or (nil, "") outside every workflow subtree. A root that is marked but not yet
// activated (a path that skipped prepareChild) is activated lazily here, so its
// pool and quota still hold; a root that cannot activate yields (nil, "").
func (s *workflowScope) activationOf(nodeID string) (*workflowActivation, string) {
	rootID := s.tree.WorkflowRootOf(nodeID)
	if rootID == "" {
		return nil, ""
	}
	s.mu.Lock()
	act, ok := s.active[rootID]
	s.mu.Unlock()
	if ok {
		return act, rootID
	}
	act, err := s.activateWorkflow(s.tree.Node(rootID))
	if err != nil {
		return nil, ""
	}
	return act, rootID
}

// backstop returns the workflow spawn backstop for a spawner at parent. It is
// lazy: each call resolves the governing activation afresh and delegates to
// backstopFor, so a spawner built before its subtree activated still enforces
// the pool, and every backstop for one root shares that activation's counter.
// Outside a workflow it never refuses.
func (s *workflowScope) backstop(parent *agent.AgentNode) func(newDepth int) error {
	return func(newDepth int) error {
		act, rootID := s.activationOf(parent.ID)
		if act == nil {
			return nil
		}
		if bs := backstopFor(s.tree, act, rootID); bs != nil {
			return bs(newDepth)
		}
		return nil
	}
}

// budget returns the spawn tool's BudgetFunc for node: the tighter of the
// workflow total and the global budget inside a workflow, else the global one.
func (s *workflowScope) budget(node *agent.AgentNode) tools.BudgetFunc {
	act, rootID := s.activationOf(node.ID)
	return budgetFor(s.tree, act, rootID)
}

// spawnTool builds node's spawn_agent around fn. Inside a workflow subtree it
// advertises that workflow's workers; outside it advertises the configured
// workflows a child may root (decision: workflows do not nest, so `workflow` is
// never offered inside a subtree).
func (s *workflowScope) spawnTool(fn tools.SpawnFunc, node *agent.AgentNode) *tools.SpawnAgentTool {
	t := tools.NewSpawnAgentToolWithBudget(fn, s.budget(node)).
		WithQuotaWarning(quotaWarningFor(s.tree, node.ID))
	if act, _ := s.activationOf(node.ID); act != nil {
		return t.WithWorkers(act.workerNames())
	}
	return t.WithWorkflows(s.names())
}

// checkRequest validates a spawn request's workflow and worker names before any
// node exists or any quota is spent. A worker resolves against the requested
// workflow when one is named, else against the parent's governing workflow;
// outside every workflow a worker is ignored (as before). A nested activation is
// left to the Spawner (agent.ErrNestedWorkflow).
func (s *workflowScope) checkRequest(parent *agent.AgentNode, workflow, worker string) error {
	if workflow != "" {
		if err := s.check(workflow); err != nil {
			return err
		}
		if worker != "" {
			if _, ok := s.workflows[workflow].Workers[worker]; !ok {
				return fmt.Errorf("unknown worker %q", worker)
			}
		}
		return nil
	}
	if worker == "" {
		return nil
	}
	if act, _ := s.activationOf(parent.ID); act != nil {
		if _, ok := act.cfg.Workers[worker]; !ok {
			return fmt.Errorf("unknown worker %q", worker)
		}
	}
	return nil
}

// spawnFunc is spawnFuncFrom with the request validated first, so an unknown
// workflow or worker is a tool error that creates no node and spends no quota.
func (s *workflowScope) spawnFunc(sp *agent.Spawner, parent *agent.AgentNode) tools.SpawnFunc {
	inner := spawnFuncFrom(sp, s.tree.Scheduler(), parent)
	return func(ctx context.Context, req tools.SpawnRequest) (tools.SpawnHandle, error) {
		if err := s.checkRequest(parent, req.Workflow, req.Worker); err != nil {
			return nil, err
		}
		return inner(ctx, req)
	}
}

// prepareChild is the child builder's first step: it activates the workflow a
// workflow-root child roots, then resolves the child's tools and model pin
// against the governing workflow. Outside every workflow the requested subset
// passes through unchanged and any worker is ignored. It repeats the name checks
// for paths that call Spawner.Spawn directly (pipeline steps).
func (s *workflowScope) prepareChild(opts agent.SpawnOpts, child *agent.AgentNode) (childPlan, error) {
	if opts.Workflow != "" {
		if _, err := s.activateWorkflow(child); err != nil {
			return childPlan{}, err
		}
	}
	act, _ := s.activationOf(child.ID)
	if act == nil {
		return childPlan{tools: opts.Tools}, nil
	}
	rt, err := resolveWorkerTools(act.cfg, opts.Worker, opts.Tools)
	if err != nil {
		return childPlan{}, err
	}
	plan := childPlan{tools: rt, bound: opts.Worker != ""}
	if plan.bound {
		plan.model = act.cfg.Workers[opts.Worker].Model
	}
	return plan, nil
}

// workflowRunner runs fn with a spawner rooted at a fresh coordinator node that
// roots workflow (change 0091): the pipeline_run workflow path. label names the
// coordinator node; fn's steps spawner makes every step a child of it, so the
// steps queue into, count against and resolve workers from that workflow.
type workflowRunner func(ctx context.Context, label, workflow string, fn func(ctx context.Context, steps *agent.Spawner) (string, error)) (string, error)

// runner returns the workflowRunner for pipelines started at parent. The
// coordinator is one ordinary spawn of parent (so it is admitted into parent's
// pool and counts against the tree-global max_spawns), marked as the workflow
// root; its builder runs no model and only drives fn. A workflow requested from
// inside a workflow subtree is refused by the Spawner (agent.ErrNestedWorkflow),
// and an unknown name is refused before any node exists.
func (s *workflowScope) runner(makeSpawner func(*agent.AgentNode, int) *agent.Spawner, parent *agent.AgentNode) workflowRunner {
	sched := s.tree.Scheduler()
	return func(ctx context.Context, label, workflow string, fn func(ctx context.Context, steps *agent.Spawner) (string, error)) (string, error) {
		if err := s.check(workflow); err != nil {
			return "", err
		}
		coord := makeSpawner(parent, parent.Depth).WithBuilder(func(ctx context.Context, _ agent.SpawnOpts, node *agent.AgentNode, _ *agent.AgentTree) (string, error) {
			if _, err := s.activateWorkflow(node); err != nil {
				return "", err
			}
			// The coordinator runs no model: it must not hold its scheduler slot
			// while its steps queue for one, or the run deadlocks under
			// max_concurrent (learning slot-cap-yield-while-blocked-on-children).
			sched.YieldSlot(node)
			out, err := fn(ctx, makeSpawner(node, node.Depth))
			if !sched.UnyieldSlot(ctx, node) {
				return "", ctx.Err()
			}
			return out, err
		})
		h, err := coord.Spawn(ctx, agent.SpawnOpts{Label: label, Task: label, Workflow: workflow})
		if err != nil {
			return "", err
		}
		d := h.Wait()
		return d.Result, d.Err
	}
}
