// Command fuse is a multi-model agent harness CLI.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/banner"
	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/session"
	"github.com/ethanhinson/fuse/internal/tools"
	"github.com/ethanhinson/fuse/internal/tui"
	"github.com/ethanhinson/fuse/internal/version"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is the testable entry point. It returns a process exit code.
func run(args []string, stdout, stderr io.Writer) int {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(stderr, "config error: %v\n", err)
		return 1
	}
	reg := registryFromConfig(cfg)

	// Subcommand dispatch (only the ones that are not the default task run).
	if len(args) > 0 {
		switch args[0] {
		case "models":
			for _, name := range reg.Names() {
				mc, _ := reg.Resolve(name)
				marker := "  "
				if name == reg.Default {
					marker = "* "
				}
				fmt.Fprintf(stdout, "%s%s\t%s\n", marker, name, mc.ID)
			}
			return 0
		case "shell":
			return runShell(args[1:], cfg, reg, stdout, stderr)
		case "mcps":
			return runMCPs(args[1:], cfg, stdout, stderr)
		case "mcp-server":
			return runMCPServer(args[1:], cfg, stdout, stderr)
		case "help":
			banner.Print(stdout, version.Version)
			fmt.Fprintln(stdout, "commands:")
			fmt.Fprintln(stdout, "  fuse <task>       run an agent on a one-shot task")
			fmt.Fprintln(stdout, "  fuse models       list configured model aliases")
			fmt.Fprintln(stdout, "  fuse shell        start an interactive agent shell")
			fmt.Fprintln(stdout, "  fuse mcps         list connected MCP servers")
			fmt.Fprintln(stdout, "  fuse help         show this help")
			return 0
		}
	}

	fs := flag.NewFlagSet("fuse", flag.ContinueOnError)
	fs.SetOutput(stderr)
	modelAlias := fs.String("model", "", "model alias to run (default: config default)")
	verbose := fs.Bool("verbose", false, "render full tool args and output")
	traceFile := fs.String("trace", "", "write raw gateway JSON to this file (- = stderr)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) == 0 {
		banner.Print(stderr, version.Version)
		fmt.Fprintln(stderr, "usage: fuse [--model NAME] [--verbose] [--trace FILE] \"<task>\"")
		return 2
	}
	task := rest[0]

	var traceW io.Writer
	switch *traceFile {
	case "":
		// no trace
	case "-":
		traceW = &syncWriter{w: stderr}
	default:
		w, closeTrace := openTraceWriter(*traceFile)
		if w == nil {
			fmt.Fprintf(stderr, "trace: cannot open %s\n", *traceFile)
			return 1
		}
		defer closeTrace()
		traceW = w
	}

	// Spill dir for truncated tool outputs (recoverable via grep/read_file).
	tools.SetSpillDir(filepath.Join(filepath.Dir(session.DefaultLogDir()), "tool-output"))

	// Build a tool registry with spawn_agent wired up for one-shot mode.
	toolReg := defaultToolRegistry(cfg.Research, nil)
	tree := agent.NewAgentTree(*modelAlias, *modelAlias)
	rootNode := tree.Node(tree.RootID())

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
				childToolReg.Register(tools.NewSpawnAgentTool(makeSpawnFunc(childNode, childNode.Depth)))

				r := tui.NewRenderer(stdout, *verbose)
				modelID := opts.ModelID
				if modelID == "" {
					modelID = *modelAlias
				}
				var a *agent.Agent
				var aerr error
				if opts.SystemPrompt != "" {
					a, aerr = buildChildAgent(cfg, reg, modelID, r, opts.SystemPrompt, childToolReg, permissions.AlwaysApprove, traceW, opts.Label)
				} else {
					a, aerr = buildAgentWithRendererAndTrace(cfg, reg, modelID, r, *verbose, spawnAgentBlock, childToolReg, permissions.AlwaysApprove, traceW, opts.Label)
				}
				if aerr != nil {
					return "", aerr
				}
				msgs, rerr := a.Run(ctx, []model.Message{{Role: "user", Content: opts.Task}})
				return childResult(msgs, rerr)
			}),
		)
		return func(ctx context.Context, label, task, systemPrompt, modelID string, toolsList []string) (string, error) {
			opts := agent.SpawnOpts{
				Label:        label,
				Task:         task,
				SystemPrompt: systemPrompt,
				ModelID:      modelID,
				Tools:        toolsList,
			}
			handle, herr := spawner.Spawn(ctx, opts)
			if herr != nil {
				return "", herr
			}
			// Yield this agent's spawn slot while blocked on the child —
			// parents holding slots while their children queue is a deadlock.
			tree.YieldSlot(parentNode)
			done := handle.Wait()
			if !tree.UnyieldSlot(ctx, parentNode) {
				return "", ctx.Err()
			}
			return done.Result, done.Err
		}
	}
	toolReg.Register(tools.NewSpawnAgentTool(makeSpawnFunc(rootNode, 0)))

	a, modelID, err := buildAgentCore(cfg, reg, *modelAlias, tui.NewRenderer(stdout, *verbose), spawnAgentBlock, traceW, "root", toolReg, permissions.AlwaysApprove)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	_ = modelID

	_, err = a.Run(context.Background(), []model.Message{{Role: "user", Content: task}})
	if err != nil {
		fmt.Fprintf(stderr, "run error: %v\n", err)
		return 1
	}
	return 0
}
