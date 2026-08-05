package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/session"
	"github.com/ethanhinson/fuse/internal/skills"
	"github.com/ethanhinson/fuse/internal/tools"
	"github.com/ethanhinson/fuse/internal/tui"
)

// runShell loads skills, builds provider, and runs the interactive bubbletea
// TUI. It parses a minimal set of pre-flags (--model NAME, --verbose) and
// then hands control to the ShellModel. One-shot mode (cmd/fuse/main.go) is
// unaffected.
func runShell(args []string, cfg config.Config, reg *model.Registry, stdout, stderr io.Writer) int {
	alias := reg.Default
	verbose := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--model":
			if i+1 < len(args) {
				alias = args[i+1]
				i++
			}
		case "--verbose":
			verbose = true
		}
	}

	skillDirs := skills.DefaultDirs()

	set, err := skills.LoadWithEmbedded(skillDirs)
	if err != nil {
		fmt.Fprintf(stderr, "skills error: %v\n", err)
		return 1
	}
	skillBlock := set.SystemPromptBlock()

	toolReg, err := buildSessionRegistryNoMCP(cfg, set.Lookup)
	if err != nil {
		fmt.Fprintf(stderr, "session registry error: %v\n", err)
		return 1
	}

	builtins := tui.NewBuiltinProvider()

	skillProv, err := tui.NewSkillProvider(skillDirs)
	if err != nil {
		log.Printf("skill provider: %v", err)
		skillProv = nil
	}

	mcpProv, err := tui.NewMCPProvider(config.Path(), cfg, toolReg)
	if err != nil {
		log.Printf("mcp provider: %v", err)
		mcpProv = nil
	}

	var slashReg *tui.SlashRegistry
	if mcpProv != nil && skillProv != nil {
		slashReg = tui.NewSlashRegistry(builtins, skillProv, mcpProv)
	} else if mcpProv != nil {
		slashReg = tui.NewSlashRegistry(builtins, mcpProv)
	} else if skillProv != nil {
		slashReg = tui.NewSlashRegistry(builtins, skillProv)
	} else {
		slashReg = tui.NewSlashRegistry(builtins)
	}
	defer slashReg.Close()

	// Inject aggressive spawn_agent guidance (shared with one-shot mode).
	skillBlock += spawnAgentBlock

	traceFile := ""
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--trace" {
			traceFile = args[i+1]
			break
		}
	}
	// One shared, mutex-guarded trace writer for the whole session so root and
	// child agents all land in the same file with per-agent labels.
	traceW, closeTrace := openTraceWriter(traceFile)
	defer closeTrace()

	build := func(a string, r agent.Renderer, approve permissions.ApprovalFunc) (*agent.Agent, error) {
		return buildAgentWithRendererAndTrace(cfg, reg, a, r, verbose, skillBlock, toolReg, approve, traceW, "root")
	}

	glamourStyle := os.Getenv("GLAMOUR_STYLE")
	if glamourStyle == "" {
		glamourStyle = "dark"
	}

	// Spill dir: full copies of truncated tool outputs, recoverable by the
	// model via grep/read_file (see docs/designs/context-management.md).
	tools.SetSpillDir(filepath.Join(filepath.Dir(session.DefaultLogDir()), "tool-output"))

	// Session log: sweep stale files and open a fresh log.
	logDir := session.DefaultLogDir()
	go session.SweepOld(logDir, 7*24*time.Hour)
	sessLog, serr := session.NewLogger(logDir)
	if serr != nil {
		log.Printf("session log: %v", serr)
		sessLog = nil
	} else {
		defer sessLog.Close()
	}

	// Agent tree for subagent tracking. The tree-global spawn budget backstops
	// runaway fan-out; its count feeds the budget line injected into results.
	tree := agent.NewAgentTree(alias, alias)
	tree.SetMaxSpawns(cfg.Agents.MaxSpawns)
	rootNode := tree.Node(tree.RootID())

	// SpawnFunc factory — self-referential so child agents get their own spawner.
	var makeSpawnFunc func(parentNode *agent.AgentNode, parentDepth int) tools.SpawnFunc
	makeSpawnFunc = func(parentNode *agent.AgentNode, parentDepth int) tools.SpawnFunc {
		spawner := agent.NewSpawner(
			agent.WithTree(tree),
			agent.WithNode(parentNode),
			agent.WithSpawnDepth(parentDepth),
			agent.WithChildBuilder(func(ctx context.Context, opts agent.SpawnOpts, childNode *agent.AgentNode, childTree *agent.AgentTree) (string, error) {
				// Child-specific tool registry (clone or subset); unknown tool
				// names fail the spawn so the model can self-correct.
				childToolReg, terr := childToolRegistry(toolReg, opts.Tools)
				if terr != nil {
					return "", terr
				}
				// Replace spawn_agent with one wired to the child's spawner.
				childToolReg.Register(tools.NewSpawnAgentToolWithBudget(makeSpawnFunc(childNode, childNode.Depth), tree.SpawnBudget))

				r := tui.NewNodeRenderer(childNode, childTree)
				// Child agents inherit the parent's permission config (disabled tools
				// are respected) but use AlwaysApprove so they don't block on TUI
				// approval and can run truly in parallel when batched.
				childApprove := permissions.AlwaysApprove

				modelAlias := opts.ModelID
				if modelAlias == "" {
					modelAlias = alias
				}

				var a *agent.Agent
				var aerr error
				if opts.SystemPrompt != "" {
					a, aerr = buildChildAgent(cfg, reg, modelAlias, r, opts.SystemPrompt, childToolReg, childApprove, traceW, opts.Label)
				} else {
					a, aerr = buildAgentWithRendererAndTrace(cfg, reg, modelAlias, r, verbose, skillBlock, childToolReg, childApprove, traceW, opts.Label)
				}
				if aerr != nil {
					return "", aerr
				}

				history := []model.Message{{Role: "user", Content: opts.Task}}
				msgs, rerr := a.Run(ctx, history)

				if sessLog != nil {
					kind := "done"
					if rerr != nil {
						kind = "error"
					}
					_ = sessLog.Write(session.LogEntry{
						TS:       time.Now(),
						NodeID:   childNode.ID,
						ParentID: childNode.ParentID,
						Label:    childNode.Label,
						Depth:    childNode.Depth,
						Kind:     kind,
					})
				}

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

	// Register spawn_agent in the tool registry before any agent runs.
	toolReg.Register(tools.NewSpawnAgentToolWithBudget(makeSpawnFunc(rootNode, 0), tree.SpawnBudget))

	m := tui.NewShellModel(alias, verbose, glamourStyle, reg, slashReg, build)
	m = m.WithTree(tree)

	// Start the 250ms dirty-node flusher; the same ctx stops the bridges.
	flushCtx, cancelFlusher := context.WithCancel(context.Background())
	defer cancelFlusher()
	tree.StartDirtyFlusher(flushCtx)

	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion(), tea.WithOutput(stdout))
	// Pump agent events, tree updates, and registry reloads into the program.
	// Program.Send is safe before Run (it blocks until the loop consumes) and
	// returns without delivering once the program has quit.
	tui.StartBridges(flushCtx, p, m.Channel(), tree, slashReg)
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(stderr, "tui error: %v\n", err)
		return 1
	}
	return 0
}
