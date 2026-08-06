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
	tree.SetMaxSpawns(cfg.Agents.MaxSpawns)
	rootNode := tree.Node(tree.RootID())

	// Self-referential spawn factory, mirroring shell.go. The only difference
	// from production: each child renders through a MultiRenderer that feeds
	// both the tree (for the hierarchy snapshot) and the shared recorder Log
	// (for the event transcript). Nothing about the agents themselves is faked.
	var makeSpawnFunc func(parentNode *agent.AgentNode, depth int) tools.SpawnFunc
	makeSpawnFunc = func(parentNode *agent.AgentNode, depth int) tools.SpawnFunc {
		spawner := agent.NewSpawner(
			agent.WithTree(tree),
			agent.WithNode(parentNode),
			agent.WithSpawnDepth(depth),
			agent.WithChildBuilder(func(ctx context.Context, opts agent.SpawnOpts, childNode *agent.AgentNode, childTree *agent.AgentTree) (string, error) {
				childToolReg, terr := childToolRegistry(toolReg, opts.Tools)
				if terr != nil {
					return "", terr
				}
				if childNode.Depth >= agent.MaxDepth || !shouldWireChildSpawn(opts.Tools) {
					// Depth strip (static): a child at MaxDepth can never spawn.
					// Folded-in fix (change 0034): a parent that omits spawn_agent
					// from its requested tools subset withholds it from the child.
					childToolReg.Unregister("spawn_agent")
				} else {
					childToolReg.Register(tools.NewSpawnAgentToolWithBudget(makeSpawnFunc(childNode, childNode.Depth), tree.SpawnBudget))
				}

				label := childNode.Label
				if label == "" {
					label = "child"
				}
				r := tui.NewMultiRenderer(
					tui.NewNodeRenderer(childNode, childTree),
					logSink.Recorder(label),
				)

				modelID := opts.ModelID
				if modelID == "" {
					modelID = alias
				}
				var a *agent.Agent
				var aerr error
				// research-probe is headless (no TTY, AlwaysApprove) ⇒ backstopped.
				if opts.SystemPrompt != "" {
					a, aerr = buildChildAgent(cfg, reg, modelID, r, opts.SystemPrompt, childToolReg, permissions.AlwaysApprove, traceW, label, nil, false)
				} else {
					a, _, aerr = buildAgentCore(cfg, reg, modelID, r, spawnAgentBlock, traceW, label, childToolReg, permissions.AlwaysApprove, nil, false)
				}
				if aerr != nil {
					return "", aerr
				}
				a.SetStripSpawn(agent.NewStripSpawnPredicate(tree, cfg.Agents.MaxConcurrent))
				msgs, rerr := a.Run(ctx, []model.Message{{Role: "user", Content: opts.Task}})
				return childResult(msgs, rerr)
			}),
		)
		return func(ctx context.Context, label, task, systemPrompt, modelID string, toolsList []string) (string, error) {
			handle, herr := spawner.Spawn(ctx, agent.SpawnOpts{
				Label: label, Task: task, SystemPrompt: systemPrompt, ModelID: modelID, Tools: toolsList,
			})
			if herr != nil {
				return "", herr
			}
			tree.YieldSlot(parentNode)
			done := handle.Wait()
			if !tree.UnyieldSlot(ctx, parentNode) {
				return "", ctx.Err()
			}
			return done.Result, done.Err
		}
	}
	toolReg.Register(tools.NewSpawnAgentToolWithBudget(makeSpawnFunc(rootNode, 0), tree.SpawnBudget))

	// Root renderer: tree node + recorder, same MultiRenderer shape as children.
	rootR := tui.NewMultiRenderer(
		tui.NewNodeRenderer(rootNode, tree),
		logSink.Recorder("root"),
	)
	rootAgent, _, err := buildAgentCore(cfg, reg, alias, rootR, spawnAgentBlock, traceW, "root", toolReg, permissions.AlwaysApprove, nil, false)
	if err != nil {
		fmt.Fprintf(stderr, "build root agent: %v\n", err)
		return 1
	}
	rootAgent.SetStripSpawn(agent.NewStripSpawnPredicate(tree, cfg.Agents.MaxConcurrent))

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
