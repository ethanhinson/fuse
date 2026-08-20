package sandbox

import (
	"context"
	"errors"
	"os/exec"
	"sort"
	"sync"

	"github.com/ethanhinson/fuse/internal/loopauth"
)

// HandlerHost is the bounded handler identifier for the host substrate. It is a
// closed enum value used in events and metric labels.
const HandlerHost = "host"

// hostHandler runs commands directly on this machine, in this process's
// namespace. It is the OFF-SWITCH substrate: it exists only for the case where
// an operator has explicitly authorized uncontained execution through the
// file-only config, and it is never reached by default and never by fallback.
//
// Being uncontained does NOT mean being unscrubbed. The host path applies the
// exact same explicit environment allowlist as the container path; the
// difference between the substrates is isolation, not credential exposure.
type hostHandler struct{}

// newHostHandler returns the host substrate handler. It cannot fail: the host
// is always "available", which is precisely why selection — not construction —
// must be the thing that refuses to use it.
func newHostHandler() *hostHandler { return &hostHandler{} }

// Name reports the bounded handler identifier.
func (*hostHandler) Name() string { return HandlerHost }

// Acquire returns a Runner bound to p and to env. There is nothing to allocate
// on the host, so this never fails; the returned Runner is a fresh value per
// call so that no two principals can ever share one (the Runner is the thing a
// pool keys and hands out, and identity is how that key is enforced).
func (*hostHandler) Acquire(_ context.Context, p loopauth.Principal, env Env) (Runner, error) {
	return &hostRunner{
		principal: p,
		// Render once, at Acquire, from the env the caller resolved. The
		// Runner never consults the process environment afterwards, so there
		// is no code path on which an ambient variable can reach a command.
		env: renderEnv(env),
	}, nil
}

// hostRunner is one principal's host execution context.
type hostRunner struct {
	// principal is fixed at Acquire and never reassigned: a Runner belongs to
	// exactly one principal for its whole life, which is what lets the warm
	// pool re-assert ownership on checkout (see acquiredFor).
	principal loopauth.Principal

	// mu guards env. A pooled Runner is re-environed on checkout (ResetEnv)
	// while a previous Exec may still be unwinding, so the environment is not
	// write-once even though it is set-once per checkout.
	mu sync.Mutex
	// env is the COMPLETE environment every command run through this Runner
	// observes, rendered as os/exec's K=V form. It is non-nil by construction
	// (see renderEnv) because a nil exec.Cmd.Env means "inherit the parent
	// process environment".
	env []string
}

// acquiredFor reports the principal this Runner was acquired for, so a pool can
// verify — against the Runner itself, not only against its own bookkeeping —
// that it is about to hand the right context to the right principal.
func (r *hostRunner) acquiredFor() loopauth.Principal { return r.principal }

// ResetEnv re-applies a freshly resolved environment to a warm Runner.
//
// It exists for the pool's reset-on-checkout: a Runner that outlives one
// checkout must observe the CURRENT allowlist on the next, not a snapshot taken
// when it was first acquired, or a rotated credential would linger in a warm
// shell. The non-nil guarantee is preserved here exactly as at Acquire.
func (r *hostRunner) ResetEnv(env Env) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.env = renderEnv(env)
	return nil
}

// currentEnv snapshots the rendered environment. It never returns nil.
func (r *hostRunner) currentEnv() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.env == nil {
		return []string{}
	}
	return append([]string(nil), r.env...)
}

// command builds the exec.Cmd for one Exec. It is separated from Exec so the
// wiring that matters most — that Cmd.Env is set, and non-nil — is directly
// assertable in a test without running anything.
func (r *hostRunner) command(ctx context.Context, cmd string, workingDir string) *exec.Cmd {
	c := exec.CommandContext(ctx, "/bin/sh", "-c", cmd)

	// SECURITY-CRITICAL, DO NOT REMOVE OR MAKE CONDITIONAL.
	//
	// os/exec treats a nil Cmd.Env as "inherit the parent process
	// environment" — every API key, token, and cloud credential this process
	// holds would become visible to model-authored shell commands. That
	// ambient-inheritance hole is the entire reason this package exists
	// (ADR-0044). Assigning r.env unconditionally is what closes it.
	//
	// currentEnv is non-nil even when the allowlist resolved to nothing, so the
	// empty-allowlist case means "no variables at all", never "inherit". The
	// belt-and-braces re-check below makes that guarantee local to this
	// function: if a future edit to the renderer ever lets a nil through, the
	// process environment still does not leak.
	c.Env = r.currentEnv()
	if c.Env == nil {
		c.Env = []string{}
	}

	// An empty workingDir means "the process default"; os/exec rejects an
	// empty-but-set Dir differently than an unset one, so only set it when
	// there is a real path.
	if workingDir != "" {
		c.Dir = workingDir
	}
	return c
}

// Exec runs cmd through /bin/sh in this Runner's scrubbed environment.
//
// Error semantics: a command that RAN and exited non-zero, or that was killed
// by the context deadline, is a normal result reported through Output
// (ExitCode / TimedOut) with a nil error. A non-nil error means the substrate
// itself failed — the command could not be started at all. Callers therefore
// must inspect Output.ExitCode; err alone does not distinguish success.
func (r *hostRunner) Exec(ctx context.Context, cmd string, workingDir string) (Output, error) {
	c := r.command(ctx, cmd, workingDir)

	combined, err := c.CombinedOutput()
	out := Output{
		Combined: combined,
		// DeadlineExceeded is checked on the caller's context:
		// exec.CommandContext kills the process when it fires, and the
		// resulting wait status is indistinguishable from any other signal
		// death without this.
		TimedOut: errors.Is(ctx.Err(), context.DeadlineExceeded),
	}

	if err == nil {
		return out, nil
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		// The command ran and reported a status (or was signalled, which
		// ExitCode reports as -1). That is a result, not a substrate failure.
		out.ExitCode = exitErr.ExitCode()
		if out.ExitCode == 0 {
			// Signalled-but-zero cannot normally happen; refuse to report it
			// as success regardless.
			out.ExitCode = -1
		}
		return out, nil
	}

	// Could not start: a bad working directory, a missing shell, a context
	// already cancelled. Report it as a substrate failure with a non-zero code
	// so a caller that only reads ExitCode still fails closed.
	out.ExitCode = -1
	return out, err
}

// Release is a no-op: the host substrate owns no resource to hand back. It
// exists so the host handler satisfies the same lifecycle as every other
// substrate, and so callers may release unconditionally on every early-return
// path without branching on which handler they hold.
func (*hostRunner) Release(context.Context) error { return nil }

// renderEnv converts a resolved Env into os/exec's K=V slice form.
//
// It NEVER returns nil — not for a zero Env, not for a nil Allow map, not for
// an empty one — because a nil exec.Cmd.Env means "inherit the parent process
// environment". "Nothing was allowed" must render as an empty environment, not
// as the whole ambient one. Keeping the empty case explicit here is what makes
// the guarantee structural rather than incidental.
//
// The output is sorted so that env and argv goldens are stable across runs
// (Go map iteration order is randomized).
func renderEnv(env Env) []string {
	out := make([]string, 0, len(env.Allow)) // non-nil even at len 0
	for k, v := range env.Allow {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}
