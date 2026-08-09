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
	"github.com/ethanhinson/fuse/internal/mcp"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/segment"
	"github.com/ethanhinson/fuse/internal/segment/fssink"
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

	// Session log: sweep stale files. The log itself opens after the agent tree
	// exists, so it can live under the per-session directory keyed by the root
	// AgentNode.ID (change 0030).
	logDir := session.DefaultLogDir()
	go session.SweepOld(logDir, 7*24*time.Hour)

	// Agent tree for subagent tracking. The tree-global spawn budget backstops
	// runaway fan-out; its count feeds the budget line injected into results.
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
	// no rpm/tpm axis is configured (fast path). The concrete bucket also feeds the
	// agents-overlay observability surface below; agents consult it via the
	// model.RateGate interface. rateGate is the untyped-nil-safe interface handle
	// (a nil bucket ⇒ nil interface ⇒ the adapter's fast path).
	rateBucket := sessionRateBucket(cfg)
	var rateGate model.RateGate
	if rateBucket != nil {
		rateGate = rateBucket
	}
	rootNode := tree.Node(tree.RootID())

	// Session log + segment store, keyed by the root AgentNode.ID (change 0030):
	// the log lives at ~/.fuse/sessions/<root-id>/session.jsonl and the concrete
	// SegmentSink writes pre-compaction regions under that dir's segments/. The
	// sink is installed for every agent built this session via installSummarizer.
	sessLog, serr := session.NewSessionLogger(logDir, tree.RootID())
	if serr != nil {
		log.Printf("session log: %v", serr)
		sessLog = nil
	} else {
		// Surface a logging failure once, at close, rather than silently. The
		// per-entry Write on the hot child path stays fire-and-forget (Logger
		// latches the first error), so a full disk or closed file no longer
		// vanishes without a trace. See session.Logger.Err/Close.
		defer func() {
			if err := sessLog.Close(); err != nil {
				log.Printf("session log: %v", err)
			}
		}()
	}
	setActiveSegmentSink(fssink.NewFSSegmentSink(logDir, tree.RootID()))
	// Point the segment_read tool at this session's segments dir so the model can
	// recover raw pre-compaction regions the sink archived (change 0030).
	tools.SetSegmentsDir(segment.SegmentsDir(logDir, tree.RootID()))

	// One blackboard per session, shared by every agent in the tree (change 0023).
	bb := agent.NewBlackboard(tree)

	build := func(a string, r agent.Renderer, approve permissions.ApprovalFunc) (*agent.Agent, error) {
		// The interactive shell reaches a human, so max_turns unset ⇒ unlimited.
		ag, err := buildAgentWithRendererAndTrace(cfg, reg, a, r, verbose, skillBlock, toolReg, approve, traceW, "root", sessionMode, true, rateGate)
		if err != nil {
			return nil, err
		}
		// Per-turn spawn-strip brake on the root turn: same predicate the children
		// carry, so the active-cap (reversible) and budget (permanent) brakes apply
		// to root-initiated spawns too. See change 0033; unified through the
		// scheduler's visibility predicate in change 0036.
		ag.SetStripSpawn(sched.StripPredicate(rootNode.ID))
		return ag, nil
	}

	// Build the ShellModel up front so its approval channel exists before the
	// spawn factory closes over it: child/subagent approvals must route to the
	// same parent TUI channel the root turn uses, rather than bypassing the gate
	// with a blanket auto-approve.
	m := tui.NewShellModel(alias, verbose, glamourStyle, reg, slashReg, build, sessionMode, classifierConstructible(cfg))
	m = m.WithTree(tree)
	m = m.WithBlackboard(bb)
	// Hand the same shared bucket to the observability surface so the agents
	// overlay shows live rate-gate utilization (change 0036); nil is the no-gate
	// fast path and renders no rate-gate segment.
	m = m.WithRateGate(rateBucket)

	// Route MCP resource-updated pushes (change 0021) into the TUI: a subscribed
	// server's notifications/resources/updated fans a ResourceUpdatedEvent, which
	// we forward as an MCPResourceUpdatedMsg onto the model channel (StartBridges
	// pumps it into the program). The observer runs on a client read-pump
	// goroutine, so it MUST be non-blocking — a buffered send that drops when the
	// channel is momentarily full (a later push re-flags the same URI anyway; the
	// indicator is idempotent per URI). Never auto-re-reads (D2).
	if mgr := mcpProv.Manager(); mgr != nil {
		ch := m.Channel()
		mgr.OnResource(func(e mcp.ResourceUpdatedEvent) {
			select {
			case ch <- tui.MCPResourceUpdatedMsg{Server: e.Server, URI: e.URI}:
			default:
			}
		})
	}

	// The parent-channel approval func for children: same TUI channel as the
	// root turn, wrapped per-child by PrefixApproval below so the human sees
	// which subagent is asking. Enforces the configured mode instead of the old
	// blanket-auto-approve bypass.
	childBaseApprove := tui.NewTeaApprovalFunc(m.Channel())

	// SpawnFunc factory — self-referential so child agents get their own spawner.
	var makeSpawner func(parentNode *agent.AgentNode, parentDepth int) *agent.Spawner
	makeSpawnFunc := func(parentNode *agent.AgentNode, parentDepth int) tools.SpawnFunc {
		return spawnFuncFrom(makeSpawner(parentNode, parentDepth), sched, parentNode)
	}
	makeSpawner = func(parentNode *agent.AgentNode, parentDepth int) *agent.Spawner {
		return agent.NewSpawner(
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
					a, aerr = buildChildAgent(cfg, reg, modelAlias, r, opts.SystemPrompt, childToolReg, childApprove, traceW, opts.Label, sessionMode, true, rateGate)
				} else {
					a, aerr = buildAgentWithRendererAndTrace(cfg, reg, modelAlias, r, verbose, skillBlock, childToolReg, childApprove, traceW, opts.Label, sessionMode, true, rateGate)
				}
				if aerr != nil {
					return "", aerr
				}
				// Per-turn spawn-strip brake: omit spawn_agent from this child's
				// tool schemas when admission from its scope would not currently be
				// granted or queued within bound — the active-child cap queues
				// (reversible), the lifetime budget strips (permanent), the queue
				// bound strips (reversible). See change 0033, unified in change 0036.
				a.SetStripSpawn(sched.StripPredicate(childNode.ID))

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
	}

	// Register spawn_agent in the tool registry before any agent runs.
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
