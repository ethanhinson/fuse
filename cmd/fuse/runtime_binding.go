package main

import (
	"context"
	"io"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/runtime"
	"github.com/ethanhinson/fuse/internal/tools"
	"github.com/ethanhinson/fuse/internal/tui"
)

// runtime_binding.go is CLI binding #1's construction seam over internal/runtime.
// Each entry point (one-shot, shell, research-probe) assembles a runtime.Deps that
// closes over its own renderer / approval gate / tool wiring — those cmd-layer
// policy types stay in cmd/fuse and never cross into the Runtime. The Runtime
// receives only construction closures (BuildAgent / BuildChild) plus the store
// ownership hooks (BaseDir / InstallGlobalStore / Tree). The child-builder closures
// here are the ONLY cmd-site callers of a.Run() — they are the child runner the
// Runtime's Spawner invokes, not a root-loop drive (see TestNoDirectEngineDriveAtCmdSites).

// buildOneShotRuntimeDeps assembles runtime.Deps for the one-shot entry point,
// closing over the resolved cfg/reg/toolReg/renderer/approve wiring exactly as the
// pre-migration builder did. The renderer, approval gate, and tool registry stay in
// cmd/fuse — the seam receives only construction closures, never those types by
// name. BaseDir is "" so the Runtime installs a NoopStore (one-shot writes no event
// log — byte-identical to pre-migration). InstallGlobalStore bridges the package
// global so cmd-site child builders reading currentEventStore() see the same store.
func buildOneShotRuntimeDeps(cfg config.Config, reg *model.Registry, modelAlias string,
	toolReg *tools.Registry, tree *agent.AgentTree, stdout io.Writer, verbose bool, traceW io.Writer,
	rootApprove permissions.ApprovalFunc, oneShotSystemBlock string, oneShotBudget bool,
	rateGate model.RateGate) runtime.Deps {

	sched := tree.Scheduler()
	rootNode := tree.Node(tree.RootID())
	// One blackboard per session, shared by every agent in the tree (change 0023).
	bb := agent.NewBlackboard(tree)

	var makeSpawner func(parentNode *agent.AgentNode, depth int) *agent.Spawner
	makeSpawnFunc := func(parentNode *agent.AgentNode, depth int) tools.SpawnFunc {
		return spawnFuncFrom(makeSpawner(parentNode, depth), sched, parentNode)
	}
	// childBuilder is the loop's per-child runner (cloned child-builder site 1 of 3,
	// learning patch-every-cloned-child-builder). It is BOTH the ChildBuilder every
	// makeSpawner wires (so the root's spawn_agent tool fan-out is unchanged) AND the
	// runtime.Deps.BuildChild the Runtime's own Spawner uses for binding #2. Extracted
	// to a named var so the same closure backs both.
	childBuilder := func(ctx context.Context, opts agent.SpawnOpts, childNode *agent.AgentNode, childTree *agent.AgentTree) (string, error) {
		childToolReg, terr := childToolRegistry(toolReg, opts.Tools)
		if terr != nil {
			return "", terr
		}
		if childNode.Depth >= agent.MaxDepth || !shouldWireChildSpawn(opts.Tools) {
			// Depth strip (static): a child at MaxDepth can never spawn.
			// Folded-in fix (change 0034): a parent that omits spawn_agent
			// from its requested tools subset withholds it from the child.
			// Either way, drop any copy inherited from the parent's registry.
			childToolReg.Unregister("spawn_agent")
		} else {
			childToolReg.Register(tools.NewSpawnAgentToolWithBudget(makeSpawnFunc(childNode, childNode.Depth), sched.SpawnBudget).
				WithQuotaWarning(quotaWarningFor(tree, childNode.ID)))
		}
		// Blackboard tools bound to the child's provenance — always wired
		// (not spawn-gated), honoring an explicit subset that omits them.
		wireChildBlackboard(childToolReg, bb, childNode, opts.Tools)
		// pipeline_run bound to the child's own spawner — always wired unless
		// an explicit subset omits it (mirrors the blackboard wiring). (0026)
		pipeChildWorkers, pipeChildTools := pipelineSynthPalette(cfg, childToolReg)
		childSpawner := makeSpawner(childNode, childNode.Depth)
		wirePipelineTool(childToolReg,
			makePipelineRunFn(childSpawner, bb, sched, childNode),
			makePipelineSynthFn(childSpawner, bb, sched, childNode, cfg, pipeChildWorkers, pipeChildTools, traceW),
			opts.Tools)

		r := tui.NewRenderer(stdout, verbose)
		modelID := opts.ModelID
		if modelID == "" {
			modelID = modelAlias
		}
		var a *agent.Agent
		var aerr error
		// Subagent approvals route to the parent channel, prefixed so the
		// human sees which child is asking (same posture as CloneForChild).
		childApprove := permissions.PrefixApproval(opts.Label, rootApprove)
		// One-shot passes no session-mode source: gates default to
		// cfg.Permissions.Mode exactly as before this seam.
		if opts.SystemPrompt != "" {
			a, aerr = buildChildAgent(cfg, reg, modelID, r, opts.SystemPrompt, childToolReg, childApprove, traceW, opts.Label, nil, oneShotBudget, rateGate)
		} else {
			a, aerr = buildAgentWithRendererAndTrace(cfg, reg, modelID, r, verbose, oneShotSystemBlock, childToolReg, childApprove, traceW, opts.Label, nil, oneShotBudget, rateGate)
		}
		if aerr != nil {
			return "", aerr
		}
		a.SetStripSpawn(sched.StripPredicate(childNode.ID))
		a.SetExpects(opts.ExpectsSchema(), opts.ExpectsSink()) // 0042: structured-delegation return_result channel
		// Event stream wiring (change 0043) — cloned child-builder site 2 of 3.
		a.SetEventSink(currentEventStore())
		a.SetNodeIdentity(childNode.ID, childNode.ParentID, childNode.Depth)
		// spawn.start / spawn.done are emitted by the Spawner (change 0044),
		// the single choke point — no per-site emission here.
		msgs, rerr := a.Run(ctx, []model.Message{{Role: "user", Content: opts.Task}})
		// Report the RAW run error to the Spawner's spawn.done event (change
		// 0044) so its `kind` matches the direct session-log write even when
		// childResult collapses a max-turns/loop stop into a partial-success
		// string.
		opts.RunErrSink().Set(rerr)
		return childResult(msgs, rerr)
	}
	makeSpawner = func(parentNode *agent.AgentNode, depth int) *agent.Spawner {
		return agent.NewSpawner(
			agent.WithTree(tree),
			agent.WithNode(parentNode),
			agent.WithSpawnDepth(depth),
			// Spawn lifecycle events (change 0044): the Spawner is the single choke
			// point that emits spawn.start/spawn.done.
			agent.WithEventStore(currentEventStore()),
			agent.WithChildBuilder(childBuilder),
		)
	}
	toolReg.Register(tools.NewSpawnAgentToolWithBudget(makeSpawnFunc(rootNode, 0), sched.SpawnBudget).
		WithQuotaWarning(quotaWarningFor(tree, rootNode.ID)))
	// Root-node-wired blackboard tools (provenance = rootNode).
	wireRootBlackboard(toolReg, bb, rootNode)
	// Root pipeline_run bound to the root spawner (provenance = rootNode). (0026)
	rootPipeWorkers, rootPipeTools := pipelineSynthPalette(cfg, toolReg)
	rootPipeSpawner := makeSpawner(rootNode, 0)
	wirePipelineTool(toolReg,
		makePipelineRunFn(rootPipeSpawner, bb, sched, rootNode),
		makePipelineSynthFn(rootPipeSpawner, bb, sched, rootNode, cfg, rootPipeWorkers, rootPipeTools, traceW),
		nil)

	return runtime.Deps{
		Tree:               tree,
		BaseDir:            "", // NoopStore: one-shot writes no event log (byte-identical).
		MaxConcurrent:      cfg.Agents.MaxConcurrent,
		InstallGlobalStore: setActiveEventStore,
		NewToolRegistry:    func() *tools.Registry { return toolReg },
		BuildChild:         childBuilder,
		BuildAgent: func(modelID string, reg2 *tools.Registry) (*agent.Agent, string, error) {
			a, mid, err := buildAgentCore(cfg, reg, modelAlias, tui.NewRenderer(stdout, verbose), oneShotSystemBlock, traceW, "root", reg2, rootApprove, nil, oneShotBudget, rateGate)
			if err != nil {
				return nil, "", err
			}
			a.SetStripSpawn(sched.StripPredicate(rootNode.ID))
			// Event stream wiring (change 0043): the Runtime installed the store via
			// InstallGlobalStore before calling BuildAgent, so currentEventStore()
			// returns the Runtime-owned store (NoopStore for one-shot). SetEventSink is
			// also called by StartLoop with the same store — symmetric, inert here.
			a.SetEventSink(currentEventStore())
			a.SetNodeIdentity(rootNode.ID, rootNode.ParentID, rootNode.Depth)
			return a, mid, nil
		},
	}
}
