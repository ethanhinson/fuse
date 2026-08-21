// Package sandbox declares the pluggable substrate seam behind the bash tool.
//
// A Handler is a mechanism for running a shell command somewhere other than
// "wherever this process happens to be" — a container, a microVM, or (only when
// an operator has explicitly authorized it via the file-only off-switch) the
// host itself. A Runner is one acquired execution context for a single
// principal.
//
// The package is deliberately a LEAF. It may import internal/loopauth and
// internal/event; it must never import internal/agent, internal/observe, or
// internal/tools. Keeping it agent-free is what lets internal/tools depend on it
// without an import cycle, and what keeps the substrate testable without the
// agent runtime.
package sandbox

import (
	"context"

	"github.com/ethanhinson/fuse/internal/loopauth"
)

// Handler is a substrate mechanism capable of handing out execution contexts.
//
// Acquire is principal-scoped by construction: a Runner is obtained for exactly
// one principal and is never shared across principals.
type Handler interface {
	// Name reports the stable, bounded handler identifier used in events and
	// metric labels (for example "host" or "container"). It is a closed enum
	// value, never free-form text derived from config or model output.
	Name() string

	// Acquire returns an execution context for p, with env as the complete set
	// of environment variables the command may observe.
	Acquire(ctx context.Context, p loopauth.Principal, env Env) (Runner, error)
}

// Runner is a single acquired execution context. Every Runner obtained from
// Acquire must be Released exactly once, including on every early-return path.
type Runner interface {
	// Exec runs cmd through a shell inside the execution context, rooted at
	// workingDir.
	Exec(ctx context.Context, cmd string, workingDir string) (Output, error)

	// Release returns the execution context to its owner (a pool, or teardown).
	// Release is idempotent from the caller's perspective and must never block
	// indefinitely on a dead substrate.
	Release(ctx context.Context) error
}

// Output is the result of one Exec.
//
// Combined is stdout and stderr interleaved, as the bash tool has always
// reported them. Output never appears in an event payload.
type Output struct {
	Combined []byte
	ExitCode int
	TimedOut bool
}

// Env is the complete, explicit environment a command may observe.
//
// Allow is exhaustive: it is the whole environment, not an overlay on top of
// the host's. A nil Allow is never produced by this package, because on the
// host path a nil os/exec Env means "inherit the parent process environment" —
// precisely the leak this package exists to close. Callers must be able to
// treat a resolved Env as safe by construction.
type Env struct {
	Allow map[string]string
}
