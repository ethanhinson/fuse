package main

import (
	"io"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/tools"
)

// loopWiring is the per-loop tool wiring every binding's root and child builder
// shares (change 0091): the workflow scope, worker allowlists, scoped spawn_agent,
// blackboard and pipeline_run all come from one path, so no host can leave a
// workflow's pool or allowlist inert (learnings patch-every-cloned-child-builder,
// security-knob-inert-at-composition-root).
type loopWiring struct {
	cfg   config.Config
	tree  *agent.AgentTree
	bb    *agent.Blackboard
	scope *workflowScope
	// makeSpawner builds the binding's spawner at a parent node. It must carry
	// scope.backstop(parent) and the binding's child builder.
	makeSpawner func(parent *agent.AgentNode, depth int) *agent.Spawner
	// traceW receives the synthesized-pipeline trace line; nil disables it.
	traceW io.Writer
}

// childTools builds childNode's registry from base and returns its worker model pin.
//
// The workflow scope resolves the child's plan first (activating the workflow a
// workflow-root child roots, then applying the governing workflow's worker
// allowlist). The rest is the shape every cloned child builder carried before
// change 0091: the tools subset, the depth and subset spawn_agent strip, the
// child-bound blackboard and pipeline_run. The one difference is decision 3: a
// worker-bound child always gets the five blackboard tools, whatever its
// allowlist names.
func (w loopWiring) childTools(base *tools.Registry, opts agent.SpawnOpts, childNode *agent.AgentNode) (*tools.Registry, string, error) {
	plan, err := w.scope.prepareChild(opts, childNode)
	if err != nil {
		return nil, "", err
	}
	reg, err := childToolRegistry(base, plan.tools)
	if err != nil {
		return nil, "", err
	}
	if childNode.Depth >= agent.MaxDepth || !shouldWireChildSpawn(plan.tools) {
		// Depth strip (static): a child at MaxDepth can never spawn. Subset
		// strip (change 0034): a tools subset or worker allowlist that omits
		// spawn_agent withholds it. Either way, drop any inherited copy.
		reg.Unregister("spawn_agent")
	} else {
		reg.Register(w.scope.spawnTool(w.scope.spawnFunc(w.makeSpawner(childNode, childNode.Depth), childNode), childNode))
	}
	bbReq := plan.tools
	if plan.bound {
		bbReq = nil // worker-bound: the allowlist plus the always-wired blackboard tools
	}
	wireChildBlackboard(reg, w.bb, childNode, bbReq)
	w.wirePipeline(reg, childNode, plan.tools)
	return reg, plan.model, nil
}

// rootTools registers rootNode's spawn_agent, blackboard and pipeline_run on reg.
func (w loopWiring) rootTools(reg *tools.Registry, rootNode *agent.AgentNode) {
	reg.Register(w.scope.spawnTool(w.scope.spawnFunc(w.makeSpawner(rootNode, 0), rootNode), rootNode))
	wireRootBlackboard(reg, w.bb, rootNode)
	w.wirePipeline(reg, rootNode, nil)
}

// wirePipeline registers node's pipeline_run on reg, bound to node's own spawner
// and to the scope's workflow runner, honoring an explicit subset that omits it.
// Outside every workflow subtree it advertises the workflows a pipeline may
// root; inside one it does not (workflows do not nest), nor where the steps
// would exceed agent.MaxDepth. The synthesis palette is
// read before pipeline_run is registered, matching the order before change 0091.
func (w loopWiring) wirePipeline(reg *tools.Registry, node *agent.AgentNode, requested []string) {
	sched := w.tree.Scheduler()
	_, toolNames := pipelineSynthPalette(w.cfg, reg)
	sp := w.makeSpawner(node, node.Depth)
	run := w.scope.runner(w.makeSpawner, node)
	t := tools.NewPipelineRunTool(
		makePipelineRunFn(sp, w.bb, sched, node, run, w.scope.governing),
		makePipelineSynthFn(sp, w.bb, sched, node, w.cfg, toolNames, w.traceW, run, w.scope.governing),
	)
	if act, _ := w.scope.activationOf(node.ID); act == nil && pipelineWorkflowFits(node) {
		t = t.WithWorkflows(w.scope.names())
	}
	wireBuiltPipelineTool(reg, t, requested)
}
