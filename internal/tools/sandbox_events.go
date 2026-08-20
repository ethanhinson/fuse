package tools

import (
	"encoding/json"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/tools/sandbox"
)

// SandboxEventHooks bridges the sandbox pool's observer seam to a loop's event
// stream (change 0063 T8–T11).
//
// It lives HERE, not in internal/tools/sandbox, on purpose. The pool package is
// a leaf with respect to emission: it reports what happened in bounded terms
// (sandbox.AcquireInfo / sandbox.ReleaseInfo) and someone above it decides that
// those become events. This is that someone. Keeping the translation out of the
// pool is what stops the sandbox package from growing a dependency on the event
// vocabulary — and, transitively, on the projector that consumes it.
//
// store is the LOOP'S OWN sink: the runtime hands BuildAgent an EventStore
// already bound to that loop's StreamKey, so appending here lands on exactly
// that (tenant, loop) stream with no key plumbing. nodeID is the loop's root
// node — the pool is per-loop and owned by that node, and it is the correlation
// the projector reads. Both live in the ENVELOPE and are deliberately never
// duplicated into a payload, which would create a second source of truth.
//
// A nil store yields entirely inert hooks, which is the honest shape for a
// binding that has no per-loop event store (one-shot, shell, research-probe,
// mcp-server): those emit into nothing rather than into a NoopStore that would
// make the wiring look live.
//
// Hooks run on the caller's goroutine and, for reaps, on the pool's reaper, so
// the work is a marshal plus a best-effort Append — Append is documented as
// non-blocking and advisory, and an error is dropped rather than propagated
// into the substrate's lifecycle.
func SandboxEventHooks(store event.EventStore, nodeID string) sandbox.PoolHooks {
	if store == nil {
		return sandbox.PoolHooks{}
	}
	emit := func(kind event.Kind, payload any) {
		raw, err := json.Marshal(payload)
		if err != nil {
			return
		}
		_ = store.Append(event.Event{NodeID: nodeID, Kind: kind, Payload: raw})
	}
	release := func(kind event.Kind, i sandbox.ReleaseInfo) {
		emit(kind, event.SandboxReleasePayload{
			Handler:     i.Handler,
			ContainerID: i.ContainerID,
			Cause:       sandboxCause(i.Cause),
		})
	}
	return sandbox.PoolHooks{
		Acquired: func(i sandbox.AcquireInfo) {
			p := event.SandboxAcquirePayload{
				Handler:     i.Handler,
				Runtime:     i.Runtime,
				ContainerID: i.ContainerID,
				Reused:      i.Reused,
			}
			// Omitted on reuse: there was no substrate start to measure, and a
			// zero would read as an impossibly fast cold start in the histogram.
			if !i.Reused {
				p.ColdStartMS = i.ColdStart.Milliseconds()
			}
			emit(event.KindSandboxAcquire, p)
		},
		Released: func(i sandbox.ReleaseInfo) { release(event.KindSandboxRelease, i) },
		Reaped:   func(i sandbox.ReleaseInfo) { release(event.KindSandboxReap, i) },
	}
}

// sandboxCause maps the pool's closed enum onto the event package's closed
// enum. The two cannot reference each other (sandbox is the leaf), so they are
// kept in lockstep by an exhaustive switch here plus event_test.go's pinning.
//
// An unrecognised cause becomes the empty SandboxCause rather than being passed
// through: the value ends up as a metric label, and an unbounded label is how a
// closed enum stops being closed.
func sandboxCause(c sandbox.ReleaseCause) event.SandboxCause {
	switch c {
	case sandbox.CauseReleased:
		return event.SandboxCauseReleased
	case sandbox.CauseLoopEnd:
		return event.SandboxCauseLoopEnd
	case sandbox.CauseEarlyReturn:
		return event.SandboxCauseEarlyReturn
	case sandbox.CauseIdleTTL:
		return event.SandboxCauseIdleTTL
	case sandbox.CauseStaleCheckout:
		return event.SandboxCauseStaleCheckout
	default:
		return ""
	}
}
