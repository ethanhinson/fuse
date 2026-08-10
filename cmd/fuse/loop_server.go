package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/event"
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

// buildLoopServerRuntimeDeps assembles runtime.Deps for binding #2 — the ONLY true
// multi-loop binding (one process hosts N concurrent loops keyed by loop_id, change
// 0046). Unlike the single-loop CLI bindings it does NOT pre-build a tree or set
// Deps.Tree: StartLoop creates a FRESH tree per loop, and BuildAgent (the per-loop
// construction factory) builds ALL per-loop wiring — tree scheduler config, the
// blackboard, the child-builder / spawner closures, the root spawn/blackboard/
// pipeline tool registration, and the store binding — against THAT loop's own tree
// and store. So N concurrent loop.start calls never share a tree, a store, a
// scheduler, or a blackboard.
//
// It differs from the CLI bindings in three documented ways: (1) the renderer is a
// discardRenderer (no display), (2) permissions.AlwaysApprove is the auto-approve
// binding policy (no human on a TTY), and (3) BaseDir = session.DefaultLogDir() opens
// a REAL fsstore per loop so observe/attach have durable history.
func buildLoopServerRuntimeDeps(cfg config.Config, reg *model.Registry, modelAlias string,
	toolReg *tools.Registry, systemBlock string, rootApprove permissions.ApprovalFunc,
	rateGate model.RateGate) runtime.Deps {

	return runtime.Deps{
		// REAL fsstore per loop: observe/attach need durable history. The Runtime closes
		// each loop's store when its run completes — that releases the write handle and
		// closes live subscriber channels (terminating observe pumps), while Attach keeps
		// working because fsstore.Replay opens its own reader from events.jsonl.
		BaseDir:       session.DefaultLogDir(),
		MaxConcurrent: cfg.Agents.MaxConcurrent,
		// The per-loop tool registry is built fresh per loop from the same source as the
		// server's default, so each loop's root tool wiring binds to its own tree below.
		NewToolRegistry: func() *tools.Registry {
			return cloneServerToolRegistry(toolReg)
		},
		// BuildAgent is the per-loop factory (change 0046): store + tree are THIS loop's
		// own, so all wiring below is loop-local — no process-global, no cross-loop clobber.
		BuildAgent: func(store event.EventStore, tree *agent.AgentTree, modelID string, loopToolReg *tools.Registry) (*agent.Agent, agent.ChildBuilder, string, error) {
			sched := tree.Scheduler()
			sched.SetMaxSpawns(cfg.Agents.MaxSpawns)
			sched.SetQueueBound(cfg.Agents.QueueBound)
			sched.SetSessionTokens(cfg.Throughput.SessionTokens)
			rootNode := tree.Node(tree.RootID())
			// One blackboard per loop, shared by every agent in that loop's tree (change 0023).
			bb := agent.NewBlackboard(tree)

			var makeSpawner func(parentNode *agent.AgentNode, depth int) *agent.Spawner
			makeSpawnFunc := func(parentNode *agent.AgentNode, depth int) tools.SpawnFunc {
				return spawnFuncFrom(makeSpawner(parentNode, depth), sched, parentNode)
			}
			// childBuilder is the loop's per-child runner, bound to this loop's tree/store.
			childBuilder := func(ctx context.Context, opts agent.SpawnOpts, childNode *agent.AgentNode, childTree *agent.AgentTree) (string, error) {
				childToolReg, terr := childToolRegistry(loopToolReg, opts.Tools)
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

				childModelID := opts.ModelID
				if childModelID == "" {
					childModelID = modelAlias
				}
				var a *agent.Agent
				var aerr error
				// loop-server is headless (no TTY, AlwaysApprove) ⇒ subagent approvals
				// auto-approve too. No segment sink installed (nil).
				childApprove := permissions.PrefixApproval(opts.Label, rootApprove)
				if opts.SystemPrompt != "" {
					a, aerr = buildChildAgent(cfg, reg, childModelID, discardRenderer{}, opts.SystemPrompt, childToolReg, childApprove, nil, opts.Label, nil, false, rateGate, nil)
				} else {
					a, _, aerr = buildAgentCore(cfg, reg, childModelID, discardRenderer{}, systemBlock, nil, opts.Label, childToolReg, childApprove, nil, false, rateGate, nil)
				}
				if aerr != nil {
					return "", aerr
				}
				a.SetStripSpawn(sched.StripPredicate(childNode.ID))
				a.SetExpects(opts.ExpectsSchema(), opts.ExpectsSink()) // 0042: structured-delegation return_result channel
				// Event stream wiring (change 0043/0046): the child emits into THIS loop's
				// own store (threaded in as a value, not read from a global).
				a.SetEventSink(store)
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
					// Spawn lifecycle events (change 0044/0046): emitted by the Spawner choke
					// point onto THIS loop's own store.
					agent.WithEventStore(store),
					agent.WithChildBuilder(childBuilder),
				)
			}
			loopToolReg.Register(tools.NewSpawnAgentToolWithBudget(makeSpawnFunc(rootNode, 0), sched.SpawnBudget).
				WithQuotaWarning(quotaWarningFor(tree, rootNode.ID)))
			// Root-node-wired blackboard tools (provenance = rootNode).
			wireRootBlackboard(loopToolReg, bb, rootNode)
			// Root pipeline_run bound to the root spawner (provenance = rootNode). (0026)
			rootPipeWorkers, rootPipeTools := pipelineSynthPalette(cfg, loopToolReg)
			rootPipeSpawner := makeSpawner(rootNode, 0)
			wirePipelineTool(loopToolReg,
				makePipelineRunFn(rootPipeSpawner, bb, sched, rootNode),
				makePipelineSynthFn(rootPipeSpawner, bb, sched, rootNode, cfg, rootPipeWorkers, rootPipeTools, nil),
				nil)

			a, mid, err := buildAgentCore(cfg, reg, modelAlias, discardRenderer{}, systemBlock, nil, "root", loopToolReg, rootApprove, nil, false, rateGate, nil)
			if err != nil {
				return nil, nil, "", err
			}
			a.SetStripSpawn(sched.StripPredicate(rootNode.ID))
			// Event stream wiring (change 0043/0046): the root emits onto THIS loop's store.
			a.SetEventSink(store)
			a.SetNodeIdentity(rootNode.ID, rootNode.ParentID, rootNode.Depth)
			return a, childBuilder, mid, nil
		},
	}
}

// cloneServerToolRegistry produces a per-loop tool registry from the server's base
// registry so each hosted loop's root tool wiring (spawn_agent / blackboard /
// pipeline, bound to that loop's own tree) is isolated from every other loop's — no
// two loops mutate one shared registry (change 0046 multi-loop isolation).
func cloneServerToolRegistry(base *tools.Registry) *tools.Registry {
	return base.Clone()
}
