package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/probe"
	"github.com/ethanhinson/fuse/internal/session"
	"github.com/ethanhinson/fuse/internal/skills"
	"github.com/ethanhinson/fuse/internal/tools"
	"github.com/ethanhinson/fuse/internal/tui"
)

// (no package-level shims)

// runResearchProbe is the observability harness: it drives the REAL research
// flow — the embedded `research` skill, the real web_search/web_fetch tools
// against the configured provider, and the real spawn_agent fan-out — talking
// to the live gateway, but headless and fully recorded. Where `fuse shell`
// scrolls the fan-out past in a TUI and one-shot `fuse "<task>"` cannot reach a
// skill slash command at all, this prints an inspectable digest afterwards: the
// spawn tree, every search query, every fetched URL, and the root's final
// synthesized report.
//
// Usage:
//
//	fuse research-probe [--model alias] [--trace FILE] "<question>"
//
// It reuses the exact production wiring in shell.go (registry, tool set, spawn
// tree), swapping only the renderer for a probe.Recorder so nothing about the
// agent behavior is faked — this is the real thing, observed.
func runResearchProbe(args []string, cfg config.Config, reg *model.Registry, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("research-probe", flag.ContinueOnError)
	fs.SetOutput(stderr)
	modelAlias := fs.String("model", "", "model alias to drive the flow (default: config default)")
	traceFile := fs.String("trace", "", "also write raw gateway JSON to this file")
	timeout := fs.Duration("timeout", 3*time.Minute, "overall run timeout")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fmt.Fprintln(stderr, `usage: fuse research-probe [--model alias] [--trace FILE] "<question>"`)
		return 2
	}
	question := strings.Join(rest, " ")

	alias := *modelAlias
	if alias == "" {
		alias = reg.Default
	}

	// Trace writer (optional): the same syncWriter-wrapped file the shell uses,
	// so concurrent agents never interleave inside a REQ/RESP block.
	var traceW io.Writer
	if *traceFile != "" {
		w, closeTrace := openTraceWriter(*traceFile)
		if w == nil {
			fmt.Fprintf(stderr, "trace: cannot open %s\n", *traceFile)
			return 1
		}
		defer closeTrace()
		traceW = w
	}

	// Spill dir for truncated tool outputs, exactly as the real shell sets it.
	tools.SetSpillDir(filepath.Join(filepath.Dir(session.DefaultLogDir()), "tool-output"))

	// Load skills WITH the embedded research skill folded in, so the flow the
	// probe drives is byte-for-byte the one /research runs.
	set, err := skills.LoadWithEmbedded(skills.DefaultDirs())
	if err != nil {
		fmt.Fprintf(stderr, "skills error: %v\n", err)
		return 1
	}
	research, ok := set.Lookup("research")
	if !ok {
		fmt.Fprintln(stderr, "research skill not found — the embedded skill failed to load")
		return 1
	}

	// Real tool registry: web_search/web_fetch backed by the configured
	// provider (lazy — a missing key surfaces the first time a tool runs), plus
	// the skill tool and codeindex tools.
	toolReg := defaultToolRegistry(cfg.Research, set.Lookup)

	// The recorder Log: one shared sink; each agent gets its own Recorder.
	logSink := probe.NewLog()

	// Agent tree, so the spawn hierarchy is captured for the summary. The
	// tree-global spawn budget is what the injected budget line reports.
	tree := agent.NewAgentTreeWithConcurrency(alias, alias, cfg.Agents.MaxConcurrent)
	// The Scheduler is the single admission/slot/budget authority (change 0036):
	// set the lifetime budget on it and route slot yield/unyield through it.
	sched := tree.Scheduler()
	sched.SetMaxSpawns(cfg.Agents.MaxSpawns)
	// queue_bound (change 0036): 0/unset ⇒ the scheduler keeps its 2.0 default.
	sched.SetQueueBound(cfg.Agents.QueueBound)
	// session token ceiling (change 0036): 0/unset ⇒ no ceiling enforced.
	sched.SetSessionTokens(cfg.Throughput.SessionTokens)
	// Rate gate (change 0036): one shared token bucket for the session, or nil when
	// no rpm/tpm axis is configured (fast path).
	rateGate := sessionRateGate(cfg)
	rootNode := tree.Node(tree.RootID())
	// One blackboard per session, shared by every agent in the tree (change 0023).
	bb := agent.NewBlackboard(tree)

	// research-probe IS a research root: activate the research workflow on the
	// root node so its whole subtree is governed by the workflow pool and its
	// typed workers (change 0034). Absent a research workflow (config disabled
	// it) act stays nil and behavior is the pre-0034 freeform fan-out.
	var act *workflowActivation
	if wf, ok := cfg.Workflows["research"]; ok {
		rootNode.WorkflowRoot = "research"
		act = &workflowActivation{name: "research", cfg: wf, rootDepth: rootNode.Depth}
		// Register the pool policy with the scheduler at activation (change 0036).
		// Task 1 stores it; the fair queue and unified visibility predicate consume
		// it. The 0034 strip/backstop paths still enforce the pool for now, so this
		// is behavior-preserving.
		sched.RegisterPool(rootNode.ID, act.pool())
	}
	rootID := tree.RootID()

	// Self-referential spawn factory, mirroring shell.go. The only difference
	// from production: each child renders through a MultiRenderer that feeds
	// both the tree (for the hierarchy snapshot) and the shared recorder Log
	// (for the event transcript). Nothing about the agents themselves is faked.
	var makeSpawner func(parentNode *agent.AgentNode, depth int) *agent.Spawner
	makeSpawnFunc := func(parentNode *agent.AgentNode, depth int) tools.SpawnFunc {
		return spawnFuncFrom(makeSpawner(parentNode, depth), sched, parentNode)
	}
	makeSpawner = func(parentNode *agent.AgentNode, depth int) *agent.Spawner {
		return agent.NewSpawner(
			agent.WithTree(tree),
			agent.WithNode(parentNode),
			agent.WithSpawnDepth(depth),
			agent.WithSpawnBackstop(backstopFor(tree, act, rootID)),
			// Spawn lifecycle events (change 0044): emitted by the Spawner choke
			// point — cloned child-builder site 3 of 3.
			agent.WithEventStore(currentEventStore()),
			agent.WithChildBuilder(func(ctx context.Context, opts agent.SpawnOpts, childNode *agent.AgentNode, childTree *agent.AgentTree) (string, error) {
				// Resolve the effective tool list: inside a workflow a selected
				// worker's allowlist is authoritative (opts.Tools may only narrow
				// it); outside, opts.Tools is the freeform subset. (change 0034)
				effectiveTools := opts.Tools
				if act != nil {
					rt, rerr := resolveWorkerTools(act.cfg, opts.Worker, opts.Tools)
					if rerr != nil {
						return "", rerr
					}
					effectiveTools = rt
				}
				childToolReg, terr := childToolRegistry(toolReg, effectiveTools)
				if terr != nil {
					return "", terr
				}
				if childNode.Depth >= agent.MaxDepth || !shouldWireChildSpawn(effectiveTools) {
					// Depth strip (static): a child at MaxDepth can never spawn.
					// Folded-in fix (change 0034): a parent (or worker allowlist)
					// that omits spawn_agent withholds it from the child.
					childToolReg.Unregister("spawn_agent")
				} else {
					spawnTool := tools.NewSpawnAgentToolWithBudget(makeSpawnFunc(childNode, childNode.Depth), budgetFor(tree, act, rootID)).
						WithQuotaWarning(quotaWarningFor(tree, childNode.ID))
					if act != nil {
						spawnTool = spawnTool.WithWorkers(act.workerNames())
					}
					childToolReg.Register(spawnTool)
				}
				// Blackboard tools bound to the child's provenance — always wired
				// (not spawn-gated), honoring an explicit subset that omits them.
				// effectiveTools is the list childToolRegistry actually built from.
				wireChildBlackboard(childToolReg, bb, childNode, effectiveTools)
				// pipeline_run bound to the child's own spawner — always wired unless
				// an explicit subset omits it (mirrors the blackboard wiring). (0026)
				pipeChildWorkers, pipeChildTools := pipelineSynthPalette(cfg, childToolReg)
				childSpawner := makeSpawner(childNode, childNode.Depth)
				wirePipelineTool(childToolReg,
					makePipelineRunFn(childSpawner, bb, sched, childNode),
					makePipelineSynthFn(childSpawner, bb, sched, childNode, cfg, pipeChildWorkers, pipeChildTools, traceW),
					effectiveTools)

				label := childNode.Label
				if label == "" {
					label = "child"
				}
				r := tui.NewMultiRenderer(
					tui.NewNodeRenderer(childNode, childTree),
					logSink.Recorder(label),
				)

				modelID := opts.ModelID
				// A worker's optional model pin applies when the caller did not
				// pick a model explicitly (change 0034).
				if modelID == "" && act != nil && opts.Worker != "" {
					if w, ok := act.cfg.Workers[opts.Worker]; ok && w.Model != "" {
						modelID = w.Model
					}
				}
				if modelID == "" {
					modelID = alias
				}
				var a *agent.Agent
				var aerr error
				// research-probe is headless (no TTY, AlwaysApprove) ⇒ backstopped.
				if opts.SystemPrompt != "" {
					a, aerr = buildChildAgent(cfg, reg, modelID, r, opts.SystemPrompt, childToolReg, permissions.AlwaysApprove, traceW, label, nil, false, rateGate)
				} else {
					a, _, aerr = buildAgentCore(cfg, reg, modelID, r, spawnAgentBlock, traceW, label, childToolReg, permissions.AlwaysApprove, nil, false, rateGate)
				}
				if aerr != nil {
					return "", aerr
				}
				// Unified visibility predicate (change 0036): the scheduler folds
				// the global brakes, the workflow pool (when the node is inside a
				// registered subtree), and the queue bound into one rule — tighter
				// scope wins.
				a.SetStripSpawn(sched.StripPredicate(childNode.ID))
				a.SetExpects(opts.ExpectsSchema(), opts.ExpectsSink()) // 0042: structured-delegation return_result channel
				// Event stream wiring (change 0043) — cloned child-builder site 3 of 3.
				a.SetEventSink(currentEventStore())
				a.SetNodeIdentity(childNode.ID, childNode.ParentID, childNode.Depth)
				// spawn.start / spawn.done are emitted by the Spawner (change 0044).
				msgs, rerr := a.Run(ctx, []model.Message{{Role: "user", Content: opts.Task}})
				// Raw run error → Spawner spawn.done (change 0044): matches the
				// session-log `kind` even when childResult collapses a stop to success.
				opts.RunErrSink().Set(rerr)
				return childResult(msgs, rerr)
			}),
		)
	}
	rootSpawn := tools.NewSpawnAgentToolWithBudget(makeSpawnFunc(rootNode, 0), budgetFor(tree, act, rootID)).
		WithQuotaWarning(quotaWarningFor(tree, rootNode.ID))
	if act != nil {
		// The root offers the workflow's typed workers so the model selects a
		// facet-researcher per facet instead of hand-assembling a toolset.
		rootSpawn = rootSpawn.WithWorkers(act.workerNames())
	}
	toolReg.Register(rootSpawn)
	// Root-node-wired blackboard tools (provenance = rootNode).
	wireRootBlackboard(toolReg, bb, rootNode)
	// Root pipeline_run bound to the root spawner (provenance = rootNode). (0026)
	rootPipeWorkers, rootPipeTools := pipelineSynthPalette(cfg, toolReg)
	rootPipeSpawner := makeSpawner(rootNode, 0)
	wirePipelineTool(toolReg,
		makePipelineRunFn(rootPipeSpawner, bb, sched, rootNode),
		makePipelineSynthFn(rootPipeSpawner, bb, sched, rootNode, cfg, rootPipeWorkers, rootPipeTools, traceW),
		nil)

	// Root renderer: tree node + recorder, same MultiRenderer shape as children.
	rootR := tui.NewMultiRenderer(
		tui.NewNodeRenderer(rootNode, tree),
		logSink.Recorder("root"),
	)
	rootAgent, _, err := buildAgentCore(cfg, reg, alias, rootR, spawnAgentBlock, traceW, "root", toolReg, permissions.AlwaysApprove, nil, false, rateGate)
	if err != nil {
		fmt.Fprintf(stderr, "build root agent: %v\n", err)
		return 1
	}
	// The root is the workflow root: its own spawn schema is governed by the
	// workflow pool (concurrent/total) composed with the global brakes, all folded
	// into the scheduler's unified visibility predicate (change 0036). Its depth
	// (0 == rootDepth) keeps it below the pool's max_depth limit.
	rootAgent.SetStripSpawn(sched.StripPredicate(rootNode.ID))
	// Event stream wiring (change 0043): symmetric with the child + one-shot sites.
	rootAgent.SetEventSink(currentEventStore())
	rootAgent.SetNodeIdentity(rootNode.ID, rootNode.ParentID, rootNode.Depth)

	// The task the root receives is the /research skill body with the question
	// woven in as ARGUMENTS — exactly what the KindSkill slash path injects.
	task := research.Body + "\n\nARGUMENTS: " + question

	fmt.Fprintf(stderr, "research-probe: driving /research via %q against the live provider…\n", alias)
	tree.BeginTurn()
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	_, runErr := rootAgent.Run(ctx, []model.Message{{Role: "user", Content: task}})
	tree.EndTurn(runErr != nil)

	// Print the observable digest regardless of run error — a partial run is
	// exactly the thing worth inspecting.
	summary := probe.Summarize(logSink, tree)
	fmt.Fprint(stdout, summary.Report())

	if runErr != nil {
		fmt.Fprintf(stderr, "\nresearch-probe: run ended with error: %v\n", runErr)
		return 1
	}
	return 0
}
