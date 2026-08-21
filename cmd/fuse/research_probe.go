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
	"github.com/ethanhinson/fuse/internal/probe"
	"github.com/ethanhinson/fuse/internal/runtime"
	"github.com/ethanhinson/fuse/internal/session"
	"github.com/ethanhinson/fuse/internal/skills"
	"github.com/ethanhinson/fuse/internal/tools"
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
	// Sandbox substrate (ADR-0044, change 0063), resolved ONCE. hosted=false: the
	// probe is an operator-local diagnostic run, exactly like one-shot.
	sb := newSandboxService(false, stderr)
	toolReg := defaultToolRegistry(sb, cfg.Research, set.Lookup)
	// The bash tool's warm sandbox pool — its Runners and its reaper goroutine — is a
	// SESSION resource here, not a per-loop one: buildResearchProbeRuntimeDeps hands
	// every loop this ONE registry, and Registry.Clone shares TOOL POINTERS, so
	// releasing it from Deps.LoopTeardown would let one loop close a pool another live
	// loop is still using. The binding therefore carries no LoopTeardown and the
	// release happens exactly once, here, covering every early return below —
	// including a failed StartLoop, where the loop's completion goroutine never runs
	// (learning per-instance-resource-needs-teardown-on-every-early-return).
	defer func() { _ = tools.ReleaseSandboxes(context.Background(), toolReg) }()

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

	// Binding #1b (change 0045): the research-probe path drives the engine through
	// the in-process Runtime. buildResearchProbeRuntimeDeps holds the spawn factory,
	// workflow tool wiring, and MultiRenderer construction exactly as before
	// (behavior-preserving relocation); the tree is supplied via Deps.Tree so
	// probe.Summarize still sees it after h.Wait().
	// Session observability (change 0061): built ONCE here and threaded into the
	// probe's root and children through the deps seam.
	obs, closeObs, obsCode, obsOK := setupLocalObservability(context.Background(), cfg, stdout, stderr, "research-probe")
	if !obsOK {
		return obsCode
	}
	defer closeObs()

	deps := buildResearchProbeRuntimeDeps(researchProbeDepsInput{
		cfg: cfg, reg: reg, alias: alias, toolReg: toolReg, tree: tree,
		act: act, rootID: rootID, logSink: logSink, traceW: traceW, rateGate: rateGate,
		observer: obs.observer,
	})
	rt := runtime.New(deps)

	// The task the root receives is the /research skill body with the question
	// woven in as ARGUMENTS — exactly what the KindSkill slash path injects.
	task := research.Body + "\n\nARGUMENTS: " + question

	fmt.Fprintf(stderr, "research-probe: driving /research via %q against the live provider…\n", alias)
	// The probe owns the tree clock (Deps.Tree keeps it externally observed): mark
	// the turn around the loop exactly as before, so the agents snapshot renders the
	// root's running/frozen clock.
	tree.BeginTurn()
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	h, serr := rt.StartLoop(ctx, runtime.LoopConfig{Task: task, ModelID: alias})
	if serr != nil {
		tree.EndTurn(true)
		fmt.Fprintf(stderr, "build root agent: %v\n", serr)
		return 1
	}
	_, runErr := h.Wait()
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
