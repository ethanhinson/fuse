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

	// Session-mode source: the single holder both the TUI (indicator, Shift+Tab,
	// /mode in later tasks) and per-turn gate construction read, seeded from the
	// startup default. Threading it into the gate builders means each freshly
	// built gate is constructed at the current SESSION mode, so a mid-session
	// switch is picked up by the next built gate.
	sessionMode := permissions.NewSessionMode(permissions.ParseMode(cfg.Permissions.Mode))

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
	tree := agent.NewAgentTreeWithConcurrency(alias, alias, cfg.Agents.MaxConcurrent)
	tree.SetMaxSpawns(cfg.Agents.MaxSpawns)
	rootNode := tree.Node(tree.RootID())

	build := func(a string, r agent.Renderer, approve permissions.ApprovalFunc) (*agent.Agent, error) {
		// The interactive shell reaches a human, so max_turns unset ⇒ unlimited.
		ag, err := buildAgentWithRendererAndTrace(cfg, reg, a, r, verbose, skillBlock, toolReg, approve, traceW, "root", sessionMode, true)
		if err != nil {
			return nil, err
		}
		// Per-turn spawn-strip brake on the root turn: same predicate the children
		// carry, so the active-cap (reversible) and budget (permanent) brakes apply
		// to root-initiated spawns too. See change 0033.
		ag.SetStripSpawn(agent.NewStripSpawnPredicate(tree, cfg.Agents.MaxConcurrent))
		return ag, nil
	}

	// Build the ShellModel up front so its approval channel exists before the
	// spawn factory closes over it: child/subagent approvals must route to the
	// same parent TUI channel the root turn uses, rather than bypassing the gate
	// with a blanket auto-approve.
	m := tui.NewShellModel(alias, verbose, glamourStyle, reg, slashReg, build, sessionMode, classifierConstructible(cfg))
	m = m.WithTree(tree)
	// The parent-channel approval func for children: same TUI channel as the
	// root turn, wrapped per-child by PrefixApproval below so the human sees
	// which subagent is asking. Enforces the configured mode instead of the old
	// blanket-auto-approve bypass.
	childBaseApprove := tui.NewTeaApprovalFunc(m.Channel())

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
				if childNode.Depth >= agent.MaxDepth || !shouldWireChildSpawn(opts.Tools) {
					// Depth strip (static): a child at MaxDepth can never spawn.
					// Folded-in fix (change 0034): a parent that omits spawn_agent
					// from its requested tools subset withholds it from the child.
					// Either way, drop any copy inherited via Clone()/Subset().
					childToolReg.Unregister("spawn_agent")
				} else {
					// Replace spawn_agent with one wired to the child's spawner.
					childToolReg.Register(tools.NewSpawnAgentToolWithBudget(makeSpawnFunc(childNode, childNode.Depth), tree.SpawnBudget))
				}

				r := tui.NewNodeRenderer(childNode, childTree)
				// Child agents inherit the parent's permission config and route their
				// approvals to the same parent TUI channel, prefixed so the human sees
				// which subagent is asking. The configured mode is enforced for
				// children exactly as for the root (no blanket-auto-approve bypass).
				childApprove := permissions.PrefixApproval(opts.Label, childBaseApprove)

				modelAlias := opts.ModelID
				if modelAlias == "" {
					modelAlias = alias
				}

				var a *agent.Agent
				var aerr error
				// Children spawned inside the interactive shell inherit its
				// interactive posture — a human is reachable via the shell.
				if opts.SystemPrompt != "" {
					a, aerr = buildChildAgent(cfg, reg, modelAlias, r, opts.SystemPrompt, childToolReg, childApprove, traceW, opts.Label, sessionMode, true)
				} else {
					a, aerr = buildAgentWithRendererAndTrace(cfg, reg, modelAlias, r, verbose, skillBlock, childToolReg, childApprove, traceW, opts.Label, sessionMode, true)
				}
				if aerr != nil {
					return "", aerr
				}
				// Per-turn spawn-strip brake: omit spawn_agent from this child's
				// tool schemas when the active-child cap is reached (reversible)
				// or the lifetime budget is exhausted (permanent). See change 0033.
				a.SetStripSpawn(agent.NewStripSpawnPredicate(tree, cfg.Agents.MaxConcurrent))

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

		return func(ctx context.Context, req tools.SpawnRequest) (string, error) {
			opts := agent.SpawnOpts{
				Label:        req.Label,
				Task:         req.Task,
				SystemPrompt: req.SystemPrompt,
				ModelID:      req.Model,
				Tools:        req.Tools,
				Worker:       req.Worker,
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
