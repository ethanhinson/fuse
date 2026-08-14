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
	"github.com/ethanhinson/fuse/internal/runtime"
	"github.com/ethanhinson/fuse/internal/session"
	"github.com/ethanhinson/fuse/internal/skills"
	"github.com/ethanhinson/fuse/internal/tools"
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
	// Fail loudly at startup on a malformed config rather than silently defaulting
	// (e.g. a typo'd permissions.mode) or deferring the failure to the first
	// gateway call (a bad model reference). This single gate covers every
	// subcommand, since they all dispatch below this point.
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	reg := registryFromConfig(cfg)
	if err := validateModelRefs(cfg, reg); err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}

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
		case "loop-server":
			return runLoopServer(args[1:], cfg, reg, stdout, stderr)
		case "loop-serve-net":
			return runLoopServeNet(args[1:], cfg, reg, stdout, stderr)
		case "help":
			banner.Print(stdout, version.Version)
			fmt.Fprintln(stdout, "commands:")
			fmt.Fprintln(stdout, "  fuse <task>       run an agent on a one-shot task")
			fmt.Fprintln(stdout, "  fuse models       list configured model aliases")
			fmt.Fprintln(stdout, "  fuse shell        start an interactive agent shell")
			fmt.Fprintln(stdout, "  fuse research-probe \"<q>\"  run + observe the research flow headlessly")
			fmt.Fprintln(stdout, "  fuse mcps         list connected MCP servers")
			fmt.Fprintln(stdout, "  fuse loop-server  headless stdio JSON-RPC loop-control server (binding #2)")
			fmt.Fprintln(stdout, "  fuse loop-serve-net  networked Connect/protobuf (fuse.loop.v1) loop-control server (binding #3; bearer-token auth)")
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
	// The Scheduler is the single admission/slot/budget authority (change 0036):
	// set the lifetime budget on it and route slot yield/unyield through it.
	sched := tree.Scheduler()
	sched.SetMaxSpawns(cfg.Agents.MaxSpawns)
	// queue_bound (change 0036): 0/unset ⇒ the scheduler keeps its 2.0 default.
	sched.SetQueueBound(cfg.Agents.QueueBound)
	// session token ceiling (change 0036): 0/unset ⇒ no ceiling enforced.
	sched.SetSessionTokens(cfg.Throughput.SessionTokens)
	// Rate gate (change 0036): one shared token bucket for the whole session, or
	// nil when no rpm/tpm axis is configured (fast path). Every agent adapter below
	// shares it so the session honors one budget.
	rateGate := sessionRateGate(cfg)

	// Binding #1a (change 0045): the one-shot path now drives the engine through the
	// in-process Runtime. buildOneShotRuntimeDeps holds the tree/scheduler/blackboard,
	// spawn factory, and tool wiring exactly as before (behavior-preserving
	// relocation); the renderer/gate stay in cmd/fuse via BuildAgent's closure.
	deps, oneShotMCPClose := buildOneShotRuntimeDeps(cfg, reg, *modelAlias, toolReg, tree, stdout, *verbose, traceW, rootApprove, oneShotSystemBlock, oneShotBudget, rateGate, nil)
	rt := runtime.New(deps)
	h, err := rt.StartLoop(context.Background(), runtime.LoopConfig{Task: task, ModelID: *modelAlias})
	if err != nil {
		// StartLoop failed before the completion goroutine (which runs LoopTeardown) could
		// launch, so close the one-shot MCP manager here or its dialed servers +
		// read-pump/notify goroutines leak (change #59 review S2). Idempotent.
		oneShotMCPClose()
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	if _, err := h.Wait(); err != nil {
		fmt.Fprintf(stderr, "run error: %v\n", err)
		return 1
	}
	return 0
}
