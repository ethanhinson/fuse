package sandbox

import (
	"github.com/ethanhinson/fuse/internal/loopauth"
)

// HealthReason is the CLOSED enum of substrate-health causes this package can
// HONESTLY observe. It is deliberately a sandbox-local vocabulary: the wire
// names live in internal/event and the mapping between the two is made in
// internal/tools/sandbox_events.go, which is what keeps this package a leaf
// with respect to the event vocabulary (same discipline as ReleaseCause and
// AdmissionScope).
type HealthReason string

const (
	// HealthPullFailed is the image acquisition failing in prePull. The
	// substrate cannot start ANY container until a later pull succeeds, so it is
	// the earliest honest "this box's sandbox is broken" signal there is.
	HealthPullFailed HealthReason = "pull_failed"

	// HealthAcquireFailed is a cold start that could not produce a Runner at
	// all — no daemon, a rejected mount, a refused selection.
	HealthAcquireFailed HealthReason = "acquire_failed"

	// HealthOOM is a container killed by the kernel's OOM killer, recognised by
	// the conventional 128+SIGKILL(9) = 137 exit status.
	HealthOOM HealthReason = "oom"

	// HealthRuntimeExit is a container that died to some OTHER signal, or whose
	// runtime CLI itself failed to start the command. It is the residual
	// substrate-failure bucket, NOT a bucket for ordinary command failure.
	HealthRuntimeExit HealthReason = "runtime_exit"
)

// Deliberately ABSENT from this enum, and not an oversight: "unresponsive" and
// "recovered". Both presuppose a container whose liveness can be probed BETWEEN
// executions. This substrate is `docker run --rm` per Exec (see argv and
// containerRunner.Release: "There is no container to stop"), so no container
// outlives the command it ran and there is nothing to find unresponsive, nor
// anything to observe recovering. They are deferred to change #74 along with a
// real ContainerID, by the 2026-09-05 spec amendment. Emitting them from this
// substrate would mean synthesising a transition nothing observed — which is
// worse than an empty metric, because an operator would trust it.

// HealthInfo is one substrate-health transition, reported in bounded terms.
//
// Everything here is either a closed enum or an authenticated identity. There
// is deliberately NO error field: the underlying error text is unbounded and
// would end up on a metric label or in a payload that promises not to carry it.
// Reason is the whole of what a consumer learns about the cause.
type HealthInfo struct {
	// Principal is the identity the failing work belonged to. It never reaches
	// a payload; the consumer uses it for correlation only.
	Principal loopauth.Principal
	// Handler is the bounded substrate identifier (container | host | microvm).
	Handler string
	// Runtime is the bounded container CLI name, or "".
	Runtime string
	// Reason is the closed enum above.
	Reason HealthReason
	// Healthy is the direction of the transition. It is FALSE for every reason
	// this package can currently observe — the healthy direction ("recovered")
	// is exactly the deferred half — and is carried explicitly anyway so the
	// #74 emitter does not have to reshape this struct to report it.
	Healthy bool
}

// HealthHooks is the substrate-health observer seam.
//
// WHY THIS IS A SIBLING SEAM AND NOT A FOURTH PoolHooks FIELD — the tension
// change #74 named, decided here:
//
// PoolHooks' three fields are all ENTRY LIFECYCLE: a Runner was handed out,
// handed back, or taken away. Health is orthogonal to all three, in both
// directions:
//
//   - pull_failed and acquire_failed happen when there is NO entry. There is no
//     Runner to describe, so an AcquireInfo/ReleaseInfo-shaped hook would have
//     to be fired with a half-populated value that lies about a checkout.
//   - oom and runtime_exit happen MID-Exec, while the entry is checked out and
//     stays checked out. Nothing about the pool's bookkeeping changes; folding
//     this into Released would tell the projector a hand-back occurred that
//     did not, and would corrupt fuse_sandbox_active.
//
// They also arise at two different layers — the Pool observes acquire failures,
// the handler observes pull and exec failures — and a Pool-only struct cannot
// reach the second. So health is installed on the SERVICE (SetHealthHooks),
// which owns the handler and is itself the PoolSource, mirroring SetGateHooks
// exactly rather than inventing a third installation shape.
//
// Hooks are invoked without any lock held and must not block indefinitely.
type HealthHooks struct {
	// Unhealthy fires on an observed substrate-health transition. Nil is inert.
	Unhealthy func(HealthInfo)
}

// fire invokes the hook if one is installed. Nil-safe on both the field and the
// receiver so every call site can be unconditional.
func (h *HealthHooks) fire(i HealthInfo) {
	if h == nil || h.Unhealthy == nil {
		return
	}
	h.Unhealthy(i)
}

// classifyExit maps a completed run of the container CLI onto a health reason,
// and — critically — onto NO reason at all for an ordinary command failure.
//
// This is the anti-fabrication boundary of the whole emitter. `grep -q nope
// file` exiting 1, a failing build, a test suite returning 1: those are the
// command doing its job and reporting a result. They are NOT substrate health,
// and emitting on them would make fuse_sandbox_unhealthy_total a count of user
// command failures wearing an operator-facing name.
//
// What IS substrate failure:
//
//   - startErr non-nil: the runtime CLI could not run the command at all (no
//     daemon, unpullable image, rejected mount) ⇒ runtime_exit.
//   - exit 137: 128+SIGKILL. The conventional OOM-kill status, and what every
//     one of docker/nerdctl/podman reports for a memory-cap kill ⇒ oom.
//   - any other 128+N signal death (129..165) ⇒ runtime_exit. The container was
//     killed from outside rather than exiting on its own.
//
// timedOut suppresses everything: a deadline kill is the CALLER's bound firing,
// already reported as Output.TimedOut, and it arrives as a signal death that
// would otherwise be indistinguishable from a substrate kill. Counting it as
// unhealthy would turn every short-timeout bash call into a health incident.
//
// The bool result is "this is a health event"; false means emit nothing.
func classifyExit(exitCode int, startErr error, timedOut bool) (HealthReason, bool) {
	if timedOut {
		return "", false
	}
	if startErr != nil {
		return HealthRuntimeExit, true
	}
	switch {
	case exitCode == 137:
		return HealthOOM, true
	case exitCode > 128 && exitCode <= 128+64:
		return HealthRuntimeExit, true
	default:
		// Includes 0 and every ordinary non-zero exit. Not a health event.
		return "", false
	}
}
