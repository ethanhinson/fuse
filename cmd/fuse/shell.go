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
	"github.com/ethanhinson/fuse/internal/runtime"
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

	// Session observability (change 0061): construct the observe layer ONCE, here,
	// and thread the resulting observer into every agent the shell builds — root and
	// children alike. Only the entry point may call newObservability; a binding that
	// built its own would double-register the Prometheus collectors.
	obsCtx := context.Background()
	obs, obsCode, obsOK := setupShellObservability(obsCtx, cfg, stdout, stderr)
	if !obsOK {
		return obsCode
	}
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if cerr := obs.Close(sctx); cerr != nil {
			fmt.Fprintf(stderr, "shell: observability shutdown: %v\n", cerr)
		}
	}()

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

	// Tool/resource identity propagation (change #52 / #59): resolve the MCP attach
	// options through the single shared composition-root helper (spec §1) so the
	// shell wires the egress CredentialSource + complete-mediation gate + posture
	// log the same way every loop binding does — no binding attaches MCP without it.
	// The mediator permissions.Option the helper returns is applied to the shell's
	// per-turn approve gate via buildGate (which calls buildTargetMediator on the
	// same config); the manager options carry the egress seam. Inert (no source, nil
	// mediator) when no identity-propagating server is configured.
	mcpOpts, _ := mcpAttach(cfg, os.Stderr)
	mcpProv, err := tui.NewMCPProvider(config.Path(), cfg, toolReg, mcpOpts...)
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
	go session.SweepOld(logDir, 7*24*time.Hour, "*.jsonl")
	// Segment GC (change 0030): sweep pre-compaction segment archives older than
	// the 14-day window and prune their index entries, alongside the log sweep.
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
	// Segment sink (change 0030/0046): the shell's own per-session SegmentSink,
	// keyed by the root id. It is threaded per-loop through the summarizer install
	// (installSummarizer) instead of the retired setActiveSegmentSink global — the
	// shell owns one session, so this single value flows into every agent it builds.
	segmentSink := fssink.NewFSSegmentSink(logDir, tree.RootID())
	// Point the segment_read tool at this session's segments dir so the model can
	// recover raw pre-compaction regions the sink archived (change 0030).
	tools.SetSegmentsDir(segment.SegmentsDir(logDir, tree.RootID()))

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

	// One blackboard per session, shared by every agent in the tree (change 0023).
	bb := agent.NewBlackboard(tree)

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
		ag, err := buildAgentWithRendererAndTrace(cfg, reg, a, r, verbose, skillBlock, toolReg, approve, traceW, "root", sessionMode, true, rateGate, segmentSink, obs.observer)
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
		return ag, nil
	}

	// Build the ShellModel up front so its approval channel exists before the
	// spawn factory closes over it: child/subagent approvals must route to the
	// same parent TUI channel the root turn uses, rather than bypassing the gate
	// with a blanket auto-approve.
	m := tui.NewShellModel(alias, verbose, glamourStyle, reg, slashReg, build, sessionMode, classifierConstructible(cfg))
	m = m.WithTree(tree)
	m = m.WithBlackboard(bb)
	// Seed the local authorization identity (change #52) so identity-propagating
	// MCP tool calls mint a per-call credential from it.
	m = m.WithToolPrincipal(localPrincipal(cfg))
	// Segment surface (change 0030): the /agents overlay reads this session's
	// segments/ dir for the compaction indicator and "s" show-original.
	m = m.WithSegmentsDir(segment.SegmentsDir(logDir, tree.RootID()))
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
	// blanket-auto-approve bypass.
	childBaseApprove := tui.NewTeaApprovalFunc(m.Channel())

	// Binding #1b (change 0045): the shell routes engine CONSTRUCTION and store
	// ownership through the in-process Runtime's Deps seam. buildShellRuntimeDeps
	// wires the child-builder closure (cloned child-builder site 2 of 3) and
	// registers the root's spawn_agent / blackboard / pipeline tools on toolReg — a
	// behavior-preserving relocation of the block that lived here. The TUI keeps
	// ownership of rendering + turn cadence (via the build seam above), human
	// messaging, the projected-log consumer, and the segment sink (plan NOTE on
	// shell.go). The Runtime is constructed for seam symmetry / binding-#2 reuse; the
	// shell continues to open/close its own event store because the TUI — not
	// StartLoop — drives turns.
	_ = runtime.New(buildShellRuntimeDeps(shellDepsInput{
		cfg: cfg, reg: reg, alias: alias, toolReg: toolReg, tree: tree, bb: bb,
		verbose: verbose, skillBlock: skillBlock, sessionMode: sessionMode,
		humanBus: humanBus, handleReg: handleReg, sessLog: sessLog,
		traceW: traceW, rateGate: rateGate, logDir: logDir,
		eventStore: eventStore, segmentSink: segmentSink,
		childApprove: childBaseApprove, rootApprove: childBaseApprove,
		observer: obs.observer,
	}))

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

// setupLocalObservability builds the session-shared observe layer for a LOCAL
// entry point (`fuse shell`, one-shot `fuse <task>`, research-probe). It is the
// ONLY place those entry points may call newObservability: a binding that built
// its own would break the one-observer-per-session rule and double-register the
// Prometheus collectors.
//
// It differs from loop-serve-net in exactly one deliberate way (change 0061,
// settled decision 1): a metrics-endpoint bind failure warns on stderr and lets
// the run continue, because observability must never keep a local run from
// starting. An INVALID observability config still fails startup fast
// (decision 2) rather than silently degrading to a noop observer.
//
// logSink is where the structured (JSONL) logger writes when logging is enabled
// and logging.output is not "file". It MUST NOT be a writer some other component
// owns exclusively: the shell's TUI paints the alt screen on stdout, so the shell
// passes stderr here while the non-TUI entry points pass stdout.
//
// label prefixes the diagnostics so the operator sees which entry point spoke.
// The returned bool reports whether startup may proceed; when it is false the
// caller must return the accompanying exit code. The caller owns Close.
func setupLocalObservability(ctx context.Context, cfg config.Config, logSink, stderr io.Writer, label string) (*observabilityService, int, bool) {
	obs, err := newObservability(ctx, cfg, logSink)
	if err != nil {
		fmt.Fprintf(stderr, "%s: observability: %v\n", label, err)
		return nil, 1, false
	}
	// startMetricsEndpoint already no-ops when metrics are disabled or no bind is
	// configured, so there is no outer guard to duplicate here. Verifier is nil:
	// operator auth for a local scrape endpoint is an explicit non-goal, so a local
	// run that opts into metrics must use access: public.
	if err := obs.startMetricsEndpoint(ctx, nil); err != nil {
		fmt.Fprintf(stderr, "%s: metrics endpoint: %v (continuing)\n", label, err)
	}
	return obs, 0, true
}

// setupShellObservability is setupLocalObservability under the shell's label,
// with one shell-specific difference: the structured log sink is stderr, never
// stdout. runShell hands stdout to bubbletea (tea.WithOutput) and the TUI owns
// that writer for the whole session, so routing JSONL there would interleave log
// lines into the alt screen and corrupt the display. The `stdout` parameter is
// therefore accepted and deliberately not used as a sink.
func setupShellObservability(ctx context.Context, cfg config.Config, stdout, stderr io.Writer) (*observabilityService, int, bool) {
	_ = stdout
	return setupLocalObservability(ctx, cfg, stderr, stderr, "shell")
}
