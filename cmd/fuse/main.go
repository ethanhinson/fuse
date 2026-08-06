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
	"github.com/ethanhinson/fuse/internal/skills"
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
		case "research-probe":
			return runResearchProbe(args[1:], cfg, reg, stdout, stderr)
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
			fmt.Fprintln(stdout, "  fuse research-probe \"<q>\"  run + observe the research flow headlessly")
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
	approveAll := fs.Bool("approve-all", false, "auto-approve every tool call (scripted use; equivalent to mode: off for this run — a deliberate footgun)")
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

	// Approval fallback: replaces the removed blanket-auto-approve bypass so a
	// configured ask/prompt verdict is honored. Interactive TTY ⇒ y/N/a prompt;
	// piped stdin / CI ⇒ deny-by-default; --approve-all ⇒ explicit auto-approve.
	// Child/subagent approvals route to this same channel via PrefixApproval.
	// One-shot interactivity: a human is reachable only on a real TTY. This
	// drives the approval channel (y/N/a prompt) — --approve-all layers on top.
	oneShotInteractive := stdinIsTerminal()
	rootApprove := oneShotApprovalFunc(*approveAll, oneShotInteractive, os.Stdin, stderr)

	// The turn/loop BUDGET posture is distinct from the approval channel:
	// --approve-all is a scripted "don't ask me" posture, so even on a TTY it
	// resolves headless — an unset max_turns backstops at 100, and the doom-loop
	// hook stays nil so a trip aborts instead of auto-continuing forever.
	// Explicit max_turns config still wins. (0038, review CONCERN)
	oneShotBudget := oneShotBudgetInteractive(oneShotInteractive, *approveAll)

	// Spill dir for truncated tool outputs (recoverable via grep/read_file).
	tools.SetSpillDir(filepath.Join(filepath.Dir(session.DefaultLogDir()), "tool-output"))

	// Skills: load the real set (including the embedded research skill) so
	// one-shot mode can invoke a matching skill, exactly like shell mode. The
	// skill tool needs a real lookup and the skills directive must ride in the
	// system prompt — without both, `fuse "<task>"` can never call a skill.
	skillSet, serr := skills.LoadWithEmbedded(skills.DefaultDirs())
	if serr != nil {
		fmt.Fprintf(stderr, "skills error: %v\n", serr)
		return 1
	}
	oneShotSystemBlock := skillSet.SystemPromptBlock() + spawnAgentBlock

	// Build a tool registry with spawn_agent AND the skill tool wired up.
	toolReg := defaultToolRegistry(cfg.Research, skillSet.Lookup)
	tree := agent.NewAgentTreeWithConcurrency(*modelAlias, *modelAlias, cfg.Agents.MaxConcurrent)
	tree.SetMaxSpawns(cfg.Agents.MaxSpawns)
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
				if childNode.Depth >= agent.MaxDepth {
					// Depth strip (static): a child at MaxDepth can never spawn —
					// drop any copy inherited from the parent's registry.
					childToolReg.Unregister("spawn_agent")
				} else {
					childToolReg.Register(tools.NewSpawnAgentToolWithBudget(makeSpawnFunc(childNode, childNode.Depth), tree.SpawnBudget))
				}

				r := tui.NewRenderer(stdout, *verbose)
				modelID := opts.ModelID
				if modelID == "" {
					modelID = *modelAlias
				}
				var a *agent.Agent
				var aerr error
				// Subagent approvals route to the parent channel, prefixed so the
				// human sees which child is asking (same posture as CloneForChild).
				childApprove := permissions.PrefixApproval(opts.Label, rootApprove)
				// One-shot passes no session-mode source: gates default to
				// cfg.Permissions.Mode exactly as before this seam.
				if opts.SystemPrompt != "" {
					a, aerr = buildChildAgent(cfg, reg, modelID, r, opts.SystemPrompt, childToolReg, childApprove, traceW, opts.Label, nil, oneShotBudget)
				} else {
					a, aerr = buildAgentWithRendererAndTrace(cfg, reg, modelID, r, *verbose, oneShotSystemBlock, childToolReg, childApprove, traceW, opts.Label, nil, oneShotBudget)
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
	toolReg.Register(tools.NewSpawnAgentToolWithBudget(makeSpawnFunc(rootNode, 0), tree.SpawnBudget))

	a, modelID, err := buildAgentCore(cfg, reg, *modelAlias, tui.NewRenderer(stdout, *verbose), oneShotSystemBlock, traceW, "root", toolReg, rootApprove, nil, oneShotBudget)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	a.SetStripSpawn(agent.NewStripSpawnPredicate(tree, cfg.Agents.MaxConcurrent))
	_ = modelID

	_, err = a.Run(context.Background(), []model.Message{{Role: "user", Content: task}})
	if err != nil {
		fmt.Fprintf(stderr, "run error: %v\n", err)
		return 1
	}
	return 0
}
