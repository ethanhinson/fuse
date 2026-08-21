package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ethanhinson/fuse/internal/loopauth"
	"github.com/ethanhinson/fuse/internal/toolidentity"
	"github.com/ethanhinson/fuse/internal/tools/sandbox"
)

// errNoSubstrate is the refusal a bash tool with no sandbox Service returns.
//
// It exists because the alternative — treating "no substrate configured" as
// "run it here, with whatever environment this process holds" — is exactly the
// fail-open ADR-0044 removes. A bash tool that cannot reach a substrate is an
// UNAVAILABLE tool, and every construction path says so out loud rather than
// degrading to the host.
var errNoSubstrate = errors.New("no sandbox substrate is configured; the bash tool is unavailable")

// bashSubstrate is what the bash tool checks execution contexts out of.
//
// *sandbox.Pool is the only production implementation; the interface exists so
// the tool's own lifecycle behaviour (which principal it keys by, and whether
// it hands the Runner back on every path) is testable without a container
// daemon. It deliberately cannot widen containment: everything it can hand out
// was selected by the frozen sandbox.Service the pool sits on.
type bashSubstrate interface {
	Acquire(ctx context.Context, p loopauth.Principal) (sandbox.Runner, error)
	Release(ctx context.Context, p loopauth.Principal, r sandbox.Runner, cause sandbox.ReleaseCause) error
	Close(ctx context.Context) error
}

// bashTool runs a shell command on the sandbox substrate.
//
// It owns a warm pool over the Service it was built with. The pool is created
// LAZILY, on first use, because sandbox.NewPool starts a reaper goroutine and
// most registries built in a process (the loop-server's base registry, every
// registry a test constructs) never run a command.
type bashTool struct {
	// svc is the frozen substrate selection, resolved ONCE at the composition
	// root against a trusted repo root. The tool never re-reads config, never
	// re-selects a handler, and has no path back to the filesystem — a model
	// that writes .fuse/sandbox.local.yml mid-loop cannot change what this is.
	// Nil means no substrate: every Execute refuses (see errNoSubstrate).
	svc *sandbox.Service

	// poolOpts configure the lazily-created pool. They are what carries the
	// emission seam (sandbox.WithPoolHooks) down to a pool this tool owns: the
	// composition root holds the loop's event store, the tool holds the frozen
	// Service, and neither can construct the pool alone. They are re-applied on
	// every (re-)open, so a pool rebuilt after a teardown keeps emitting.
	//
	// They cannot widen containment: PoolOption reaches only the pool's own
	// lifecycle knobs, never the Service that decides where a command runs.
	poolOpts []sandbox.PoolOption

	mu   sync.Mutex
	pool bashSubstrate
}

// NewBash returns the bash tool bound to the process's sandbox Service.
//
// sb is the SELECTION, already made: container by default, host only where an
// operator's file-only off-switch authorized it and the hosted posture is not
// active. A nil sb is a fail-CLOSED tool, never a host fallback.
//
// opts configure the warm pool the tool creates on first use. A composition
// root that owns the loop's event store passes
// sandbox.WithPoolHooks(SandboxEventHooks(store, nodeID)) here, which is the
// ONLY path by which the pool's lifecycle reaches the durable stream the
// sandbox projection reads — without it the four sandbox event kinds are never
// emitted and every fuse_sandbox_* metric stays empty. A binding with no
// per-loop event store passes none.
func NewBash(sb *sandbox.Service, opts ...sandbox.PoolOption) Tool {
	return &bashTool{svc: sb, poolOpts: opts}
}

// NewBashWithPool returns the bash tool over a pool the CALLER owns.
//
// Use it when the pool's lifetime is managed outside the registry (a binding
// that shares one warm pool across several registries, and the teardown tests).
// The ordinary composition-root path is NewBash, which lets the tool own — and
// ReleaseSandboxes tear down — its own pool.
func NewBashWithPool(p *sandbox.Pool) Tool {
	if p == nil {
		return &bashTool{}
	}
	return &bashTool{pool: p}
}

// newBashOverSubstrate builds the tool over an arbitrary substrate (tests).
func newBashOverSubstrate(s bashSubstrate) *bashTool { return &bashTool{pool: s} }

func (*bashTool) Name() string { return "bash" }

func (*bashTool) Description() string {
	return "Execute a shell command via /bin/sh. Supports an optional timeout (seconds) and working directory."
}

func (*bashTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command":         map[string]any{"type": "string", "description": "Shell command to run"},
			"timeout_seconds": map[string]any{"type": "integer", "description": "Kill the command after this many seconds (default 120)"},
			"working_dir":     map[string]any{"type": "string", "description": "Directory to run in (default: current)"},
		},
		"required": []string{"command"},
	}
}

type bashArgs struct {
	Command        string `json:"command"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	WorkingDir     string `json:"working_dir"`
}

// substrate returns the warm pool, creating it on first use.
//
// Re-openable by design: a shell session runs many loops through ONE shared
// registry, and each loop's teardown closes the pool. Refusing afterwards would
// disable bash for the rest of the session; re-opening costs a cold start and
// changes nothing about containment, since the Service it is built over was
// frozen at startup.
func (b *bashTool) substrate() (bashSubstrate, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.pool != nil {
		return b.pool, nil
	}
	if b.svc == nil {
		return nil, errNoSubstrate
	}
	b.pool = sandbox.NewPool(b.svc, b.poolOpts...)
	return b.pool, nil
}

// CloseSandbox releases the tool's warm pool: every Runner it holds and the
// reaper goroutine. It is idempotent and safe to call from a teardown that runs
// on both an error path and the completion goroutine. See ReleaseSandboxes.
func (b *bashTool) CloseSandbox(ctx context.Context) error {
	b.mu.Lock()
	pool := b.pool
	b.pool = nil
	b.mu.Unlock()
	if pool == nil {
		return nil
	}
	return pool.Close(ctx)
}

// Execute runs a shell command on the substrate.
//
// It runs NOTHING itself: there is no exec.Command on this path any more, which
// is what makes the environment scrub and the containment decision impossible
// to bypass from here.
func (b *bashTool) Execute(ctx context.Context, args string) Result {
	var a bashArgs
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return Result{IsError: true, Output: fmt.Sprintf("bad arguments: %v", err)}
	}
	if a.Command == "" {
		return Result{IsError: true, Output: "command is required"}
	}
	if err := ctx.Err(); err != nil {
		return Result{IsError: true, Output: fmt.Sprintf("context: %v", err)}
	}

	sub, err := b.substrate()
	if err != nil {
		return Result{IsError: true, Output: fmt.Sprintf("bash is unavailable: %v", err)}
	}

	// THE POOL KEY. It comes from the authenticated principal the composition
	// root stamped on the loop context — never from `command`, never from
	// `working_dir`, never from any other model-authored field. A model that
	// could steer this key could check out another tenant's warm execution
	// context. A missing principal resolves to the zero principal LOCALLY (the
	// CLI bindings before they stamp one); it is not an error and it is not
	// guessed from the arguments.
	principal, _ := toolidentity.PrincipalFrom(ctx)

	runner, err := sub.Acquire(ctx, principal)
	if err != nil {
		// A refusal (ErrRefusedUncontained) reaches the model as an ERROR
		// result carrying the reason — never as an empty success, which would
		// read like a command that ran and printed nothing.
		return Result{IsError: true, Output: fmt.Sprintf("bash is unavailable: %v", err)}
	}

	// Release must survive a cancelled caller context: handing the Runner back
	// is bookkeeping, not work, and skipping it because the deadline fired is
	// precisely how a container leaks.
	releaseCtx := context.WithoutCancel(ctx)
	released := false
	release := func(cause sandbox.ReleaseCause) {
		if released {
			return
		}
		released = true
		_ = sub.Release(releaseCtx, principal, runner, cause)
	}
	// Backstop for the paths no branch below covers — a panic in Exec, or a
	// future early return added above the switch. Release is idempotent, so
	// the explicit calls below win and this is inert on every normal path
	// (learning per-instance-resource-needs-teardown-on-every-early-return).
	defer release(sandbox.CauseEarlyReturn)

	timeout := time.Duration(a.TimeoutSeconds) * time.Second
	if a.TimeoutSeconds == 0 {
		timeout = 120 * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	out, execErr := runner.Exec(runCtx, a.Command, a.WorkingDir)

	switch {
	case out.TimedOut || errors.Is(runCtx.Err(), context.DeadlineExceeded):
		release(sandbox.CauseEarlyReturn)
		return Result{IsError: true, Output: fmt.Sprintf("command timed out after %s\n%s", timeout, out.Combined)}

	case execErr != nil:
		// The SUBSTRATE failed — the command never started. Distinct from a
		// command that ran and failed, and never reported as success.
		release(sandbox.CauseEarlyReturn)
		return Result{IsError: true, Output: fmt.Sprintf("%s\nerror: %v", out.Combined, execErr)}

	case out.ExitCode != 0:
		// The command RAN and exited non-zero: an ordinary result, so the
		// checkout is an ordinary release and stays warm.
		release(sandbox.CauseReleased)
		return Result{IsError: true, Output: fmt.Sprintf("%s\nerror: exit status %d", out.Combined, out.ExitCode)}

	default:
		release(sandbox.CauseReleased)
		return Result{Output: string(out.Combined)}
	}
}
