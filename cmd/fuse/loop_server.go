package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/loopserver"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/runtime"
	"github.com/ethanhinson/fuse/internal/session"
	"github.com/ethanhinson/fuse/internal/skills"
	"github.com/ethanhinson/fuse/internal/tools"
)

// stdinForLoopServer is the reader loop-server drains for JSON-RPC frames. It is
// os.Stdin in production; a test swaps it for an immediately-EOF pipe to prove the
// dispatch wiring returns exit 0 without a real stdio session.
var stdinForLoopServer io.Reader = os.Stdin

// discardRenderer is binding #2's nop agent.Renderer. The loop-server has NO
// display — it is a headless JSON-RPC loop-control server, so every render call is
// discarded. It lives in cmd/fuse (the composition root), not in internal/runtime:
// the seam stays renderer-free; each binding supplies its own display (or none).
type discardRenderer struct{}

func (discardRenderer) Assistant(string)                {}
func (discardRenderer) ToolCall(string, string)         {}
func (discardRenderer) ToolResult(string, tools.Result) {}
func (discardRenderer) Errorf(string, ...any)           {}
func (discardRenderer) Tokens(int, int)                 {}

// runLoopServer implements the `fuse loop-server` subcommand (binding #2): a
// headless stdio JSON-RPC 2.0 loop-control server backed by the in-process Runtime.
// Its documented policy is AUTO-APPROVE (permissions.AlwaysApprove): there is no
// human on a TTY to gate tool calls. That is a binding CHOICE wired here at the
// composition root, not a property of the policy-free Runtime seam. Unlike the CLI
// bindings it wires a REAL fsstore (BaseDir = session.DefaultLogDir()) so
// loop.observe / loop.attach have durable history to replay on reconnect.
func runLoopServer(_ []string, cfg config.Config, reg *model.Registry, _ io.Writer, stderr io.Writer) int {
	// Auto-approve is THIS binding's policy (documented): headless loop control has
	// no human on a TTY. It is not a property of the Runtime seam.
	approve := permissions.AlwaysApprove

	skillSet, serr := skills.LoadWithEmbedded(skills.DefaultDirs())
	if serr != nil {
		fmt.Fprintf(stderr, "skills error: %v\n", serr)
		return 1
	}
	systemBlock := skillSet.SystemPromptBlock() + spawnAgentBlock
	toolReg := defaultToolRegistry(cfg.Research, skillSet.Lookup)

	// Reuse the one-shot deps wiring but with a REAL event store so observe/attach
	// have durable history. Renderer is a discarding renderer — binding #2 has no
	// display.
	deps := buildLoopServerRuntimeDeps(cfg, reg, reg.Default, toolReg, systemBlock, approve, sessionRateGate(cfg))
	rt := runtime.New(deps)

	srv := loopserver.NewServer(stdinForLoopServer, os.Stdout, rt)
	if err := srv.Serve(context.Background()); err != nil {
		fmt.Fprintf(stderr, "loop-server: %v\n", err)
		return 1
	}
	return 0
}

// buildLoopServerRuntimeDeps assembles runtime.Deps for binding #2. It mirrors
// buildOneShotRuntimeDeps's spawn-factory / tool wiring, differing in three
// documented ways: (1) the renderer is a discardRenderer (no display), (2)
// permissions.AlwaysApprove is the auto-approve binding policy (no human on a TTY),
// and (3) BaseDir = session.DefaultLogDir() opens a REAL fsstore so observe/attach
// have durable history (InstallGlobalStore bridges the package global so the
// cmd-site child builders reading currentEventStore() see it). It creates its own
// tree here so the spawn factory / root tool wiring bind to that tree's root node,
// and hands it to StartLoop via Deps.Tree.
func buildLoopServerRuntimeDeps(cfg config.Config, reg *model.Registry, modelAlias string,
	toolReg *tools.Registry, systemBlock string, rootApprove permissions.ApprovalFunc,
	rateGate model.RateGate) runtime.Deps {

	tree := agent.NewAgentTreeWithConcurrency(modelAlias, modelAlias, cfg.Agents.MaxConcurrent)
	sched := tree.Scheduler()
	sched.SetMaxSpawns(cfg.Agents.MaxSpawns)
	sched.SetQueueBound(cfg.Agents.QueueBound)
	sched.SetSessionTokens(cfg.Throughput.SessionTokens)
	rootNode := tree.Node(tree.RootID())
	// One blackboard per session, shared by every agent in the tree (change 0023).
	bb := agent.NewBlackboard(tree)

	var makeSpawner func(parentNode *agent.AgentNode, depth int) *agent.Spawner
	makeSpawnFunc := func(parentNode *agent.AgentNode, depth int) tools.SpawnFunc {
		return spawnFuncFrom(makeSpawner(parentNode, depth), sched, parentNode)
	}
	// childBuilder is the loop's per-child runner. It is BOTH the ChildBuilder every
	// makeSpawner wires (so the root's spawn_agent fan-out is unchanged) AND the
	// runtime.Deps.BuildChild the Runtime's own Spawner uses for binding #2.
	childBuilder := func(ctx context.Context, opts agent.SpawnOpts, childNode *agent.AgentNode, childTree *agent.AgentTree) (string, error) {
		childToolReg, terr := childToolRegistry(toolReg, opts.Tools)
		if terr != nil {
			return "", terr
		}
		if childNode.Depth >= agent.MaxDepth || !shouldWireChildSpawn(opts.Tools) {
			childToolReg.Unregister("spawn_agent")
		} else {
			childToolReg.Register(tools.NewSpawnAgentToolWithBudget(makeSpawnFunc(childNode, childNode.Depth), sched.SpawnBudget).
				WithQuotaWarning(quotaWarningFor(tree, childNode.ID)))
		}
		// Blackboard tools bound to the child's provenance — always wired (not
		// spawn-gated), honoring an explicit subset that omits them.
		wireChildBlackboard(childToolReg, bb, childNode, opts.Tools)
		// pipeline_run bound to the child's own spawner — always wired unless an
		// explicit subset omits it (mirrors the blackboard wiring). (0026)
		pipeChildWorkers, pipeChildTools := pipelineSynthPalette(cfg, childToolReg)
		childSpawner := makeSpawner(childNode, childNode.Depth)
		wirePipelineTool(childToolReg,
			makePipelineRunFn(childSpawner, bb, sched, childNode),
			makePipelineSynthFn(childSpawner, bb, sched, childNode, cfg, pipeChildWorkers, pipeChildTools, nil),
			opts.Tools)

		modelID := opts.ModelID
		if modelID == "" {
			modelID = modelAlias
		}
		var a *agent.Agent
		var aerr error
		// loop-server is headless (no TTY, AlwaysApprove) ⇒ subagent approvals
		// auto-approve too.
		childApprove := permissions.PrefixApproval(opts.Label, rootApprove)
		if opts.SystemPrompt != "" {
			a, aerr = buildChildAgent(cfg, reg, modelID, discardRenderer{}, opts.SystemPrompt, childToolReg, childApprove, nil, opts.Label, nil, false, rateGate)
		} else {
			a, _, aerr = buildAgentCore(cfg, reg, modelID, discardRenderer{}, systemBlock, nil, opts.Label, childToolReg, childApprove, nil, false, rateGate)
		}
		if aerr != nil {
			return "", aerr
		}
		a.SetStripSpawn(sched.StripPredicate(childNode.ID))
		a.SetExpects(opts.ExpectsSchema(), opts.ExpectsSink()) // 0042: structured-delegation return_result channel
		// Event stream wiring (change 0043): the child emits into the loop store.
		a.SetEventSink(currentEventStore())
		a.SetNodeIdentity(childNode.ID, childNode.ParentID, childNode.Depth)
		// spawn.start / spawn.done are emitted by the Spawner choke point (change 0044).
		msgs, rerr := a.Run(ctx, []model.Message{{Role: "user", Content: opts.Task}})
		// Report the RAW run error to the Spawner's spawn.done event (change 0044) so
		// its `kind` matches even when childResult collapses a stop into success.
		opts.RunErrSink().Set(rerr)
		return childResult(msgs, rerr)
	}
	makeSpawner = func(parentNode *agent.AgentNode, depth int) *agent.Spawner {
		return agent.NewSpawner(
			agent.WithTree(tree),
			agent.WithNode(parentNode),
			agent.WithSpawnDepth(depth),
			// Spawn lifecycle events (change 0044): emitted by the Spawner choke point.
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
		makePipelineSynthFn(rootPipeSpawner, bb, sched, rootNode, cfg, rootPipeWorkers, rootPipeTools, nil),
		nil)

	return runtime.Deps{
		Tree:               tree,
		BaseDir:            session.DefaultLogDir(), // REAL fsstore: observe/attach need durable history.
		MaxConcurrent:      cfg.Agents.MaxConcurrent,
		InstallGlobalStore: setActiveEventStore,
		NewToolRegistry:    func() *tools.Registry { return toolReg },
		BuildChild:         childBuilder,
		BuildAgent: func(modelID string, reg2 *tools.Registry) (*agent.Agent, string, error) {
			a, mid, err := buildAgentCore(cfg, reg, modelAlias, discardRenderer{}, systemBlock, nil, "root", reg2, rootApprove, nil, false, rateGate)
			if err != nil {
				return nil, "", err
			}
			a.SetStripSpawn(sched.StripPredicate(rootNode.ID))
			// Event stream wiring (change 0043): the Runtime installed the store via
			// InstallGlobalStore before calling BuildAgent, so currentEventStore()
			// returns the Runtime-owned fsstore.
			a.SetEventSink(currentEventStore())
			a.SetNodeIdentity(rootNode.ID, rootNode.ParentID, rootNode.Depth)
			return a, mid, nil
		},
	}
}
