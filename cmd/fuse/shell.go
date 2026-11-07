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
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/event/fsstore"
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
	// The shell always runs with its tools (change 0116): no_tools is for one-shot
	// runs, so clear it before anything is built.
	cfg = withoutNoTools(cfg, stderr, "the shell")

	// Session observability (change 0061): construct the observe layer ONCE, here,
	// and thread the resulting observer into every agent the shell builds — root and
	// children alike.
	obsCtx := context.Background()
	obs, closeObs, obsCode, obsOK := setupShellObservability(obsCtx, cfg, stderr)
	if !obsOK {
		return obsCode
	}
	// Teardown comes from the constructor (5s budget, labelled stderr warning), so
	// every early return below is covered by this single defer.
	defer closeObs()

	skillDirs := skillDirsFromConfig(cfg)

	set, err := skills.LoadWithEmbedded(skillDirs)
	if err != nil {
		fmt.Fprintf(stderr, "skills error: %v\n", err)
		return 1
	}
	// Skills listing and spawn_agent guidance, each only when its tool is
	// enabled (prompt_blocks.go).
	skillBlock := rootSystemBlock(cfg, set)

	// Sandbox substrate (ADR-0044, change 0063): resolved ONCE per shell session,
	// before the first turn.
	sb := newSandboxService(stderr)
	toolReg, err := buildSessionRegistryNoMCP(sb, cfg, set.Lookup)
	if err != nil {
		fmt.Fprintf(stderr, "session registry error: %v\n", err)
		return 1
	}
	// The bash tool's warm sandbox pool — its Runners and its reaper goroutine — is a
	// SESSION resource here, not a per-loop one: this ONE registry (and the ONE bash
	// tool in it) is shared by every turn, every child, and every loop the shell
	// binding hands out, because Registry.Clone shares TOOL POINTERS. Releasing it
	// from Deps.LoopTeardown would let one loop close a pool another live loop is
	// still using, so buildShellRuntimeDeps carries no LoopTeardown and the release
	// happens exactly once, here, covering every early return below (learning
	// per-instance-resource-needs-teardown-on-every-early-return).
	defer func() { _ = tools.ReleaseSandboxes(context.Background(), toolReg) }()

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

	// Session log: sweep stale files. The log itself opens after the agent tree
	// exists, so it can live under the per-session directory keyed by the root
	// AgentNode.ID (change 0030).
	logDir := session.DefaultLogDir()
	go session.SweepOld(logDir, 7*24*time.Hour, "*.jsonl")
	// Segment GC (change 0030): sweep legacy per-session segment archives (those
	// written under ~/.fuse/sessions before segments moved to the artifact store)
	// older than the 14-day window and prune their index entries, alongside the
	// log sweep.
	go session.SweepOldSegments(logDir, 14*24*time.Hour)

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

	// Artifact store: spilled tool output and compaction segments land under
	// ~/.fuse/artifacts/_default/<root-id>/. The shell drives turns itself, so
	// StartLoop never binds its registry: bind it here, as soon as the root id
	// exists and before anything clones toolReg (every child registry is a Clone
	// or Subset of it and so inherits this binding). Nothing between
	// buildSessionRegistryNoMCP and here clones it.
	//
	// Segment sink (change 0030/0046): the shell's own per-session SegmentSink,
	// built by fssink.Open together with that binding (one archive code path). It is
	// threaded per-loop through the summarizer install (installSummarizer)
	// instead of the retired setActiveSegmentSink global — the shell owns one
	// session, so this single value flows into every agent it builds.
	// segment_read and the /agents overlay read the same binding back.
	segmentSink, arts := fssink.Open(filepath.Join(filepath.Dir(logDir), "artifacts"), tree.RootID())
	toolReg.WithArtifacts(arts.Store, arts.Tenant, arts.Loop)

	// Session log, keyed by the root AgentNode.ID (change 0030): the log lives at
	// ~/.fuse/sessions/<root-id>/session.jsonl.
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
	// Loop event stream (change 0043/0046): open the per-session EventStore next to
	// the segment sink, keyed by the same root id. Every agent built this session
	// emits its loop events here. It flows per-loop through the Deps store holder and
	// the root build closure below instead of the retired currentEventStore() global.
	// Best-effort: a failure to open leaves the store nil ⇒ the holder's no-op default.
	var eventStore event.EventStore = event.NoopStore{}
	storeOpened := false
	if es, eerr := fsstore.NewFSEventStore(logDir, tree.RootID()); eerr != nil {
		log.Printf("event store: %v", eerr)
	} else {
		eventStore = es
		storeOpened = true
		defer func() {
			if cerr := es.Close(); cerr != nil {
				log.Printf("event store: %v", cerr)
			}
		}()
	}
	// Session log re-expressed as a CONSUMER of the event stream (change 0043,
	// "log adapts"): a subscriber projects spawn.done events into the exact current
	// session.jsonl LogEntry format and writes them to a parallel projected log,
	// run transiently alongside the direct sessLog.Write and verified byte-identical
	// (the direct writes are deleted in a trivial follow-up once equivalence holds).
	// Only wire it when a REAL store opened — the no-op default has no live channel.
	if storeOpened && sessLog != nil {
		stopConsumer := startProjectedLogConsumer(eventStore, logDir, tree.RootID())
		defer stopConsumer()
	}

	// Human-messaging substrate (ADR-0022): a per-node message bus, an @handle
	// registry (root registered up front), and an async advisory router backed by
	// a cheap model. Children register their handles and receive their own
	// injector in the spawn factory below.
	humanBus := agent.NewHumanBus(tree)
	handleReg := agent.NewHandleRegistry()
	handleReg.Register(rootNode.ID, alias)
	humanRouter := newHumanRouter(cfg, reg)

	build := func(a string, r agent.Renderer, approve permissions.ApprovalFunc) (*agent.Agent, error) {
		// The interactive shell reaches a human, so max_turns unset ⇒ unlimited.
		// segmentSink is threaded per-loop (change 0046) rather than read from a global.
		// approve is the TUI channel: a person answers it, so it is human provenance.
		ag, err := buildAgentWithRendererAndTrace(cfg, reg, a, r, verbose, skillBlock, cfg.Instructions, toolReg, permissions.Human(approve), traceW, "root", sessionMode, true, rateGate, segmentSink, obs.observer)
		if err != nil {
			return nil, err
		}
		// Per-turn spawn-strip brake on the root turn: same predicate the children
		// carry, so the active-cap (reversible) and budget (permanent) brakes apply
		// to root-initiated spawns too. See change 0033; unified through the
		// scheduler's visibility predicate in change 0036.
		ag.SetStripSpawn(sched.StripPredicate(rootNode.ID))
		// Root's human-message injector: drains humanq/<root> at each turn boundary.
		ag.SetHumanInjector(agent.NewHumanInjector(rootNode.ID, humanBus))
		// Event stream wiring (change 0043/0046): the root agent emits its loop events
		// onto the shell's own store (threaded per-loop, not read from a global),
		// tagged with the root node identity.
		ag.SetEventSink(eventStore)
		ag.SetNodeIdentity(rootNode.ID, rootNode.ParentID, rootNode.Depth)
		ag.SetUsageSink(rootNode.UpdateTokens) // the agent owns usage accounting (change 0090)
		ag.SetCallGate(func() error { return sched.TokenQuotaDenied(rootNode.ID) })
		return ag, nil
	}

	// Build the ShellModel up front so its approval channel exists before the
	// spawn factory closes over it: child/subagent approvals must route to the
	// same parent TUI channel the root turn uses, rather than bypassing the gate
	// with a blanket auto-approve.
	m := tui.NewShellModel(alias, verbose, glamourStyle, reg, slashReg, build, sessionMode, classifierConstructible(cfg))
	m = m.WithTree(tree)
	// "n" in the models editor lists what the gateway serves.
	m = m.WithModelLister(gatewayAdapter(cfg, nil).ListModels)
	m = m.WithCatalogLister(func(ctx context.Context) ([]model.CatalogModel, error) {
		return model.FetchOpenRouterCatalog(ctx, model.OpenRouterBaseURL, nil)
	})
	// Segment surface (change 0030): the /agents overlay reads this session's
	// archived segments for the compaction indicator and "s" show-original.
	m = m.WithSegments(segment.NewArtifactSource(toolReg.Artifacts()))
	m = m.WithHumanMessaging(humanBus, handleReg, humanRouter)
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
	// blanket-auto-approve bypass. A person answers the TUI channel, so it is a
	// human approver.
	childBaseApprove := permissions.Human(tui.NewTeaApprovalFunc(m.Channel()))

	// The TUI drives turns and owns rendering, human messaging, the projected-log
	// consumer and the segment sink; this wires the child builder and root spawn_agent.
	wireShellTools(shellDepsInput{
		cfg: cfg, reg: reg, alias: alias, toolReg: toolReg, tree: tree,
		verbose: verbose, skillBlock: skillBlock, sessionMode: sessionMode,
		humanBus: humanBus, handleReg: handleReg, sessLog: sessLog,
		traceW: traceW, rateGate: rateGate,
		eventStore: eventStore, segmentSink: segmentSink,
		childApprove: childBaseApprove,
		observer:     obs.observer,
	})

	// ask_user: reaches the human through the same TUI channel as approvals, so
	// the model can pose a structured multiple-choice question and block for the
	// answer, which returns as the tool result (and thus into context). Registered
	// here because it depends on the ShellModel channel built above.
	toolReg.Register(tools.NewAskUserTool(tui.NewTeaAskFunc(m.Channel())))

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
