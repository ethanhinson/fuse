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

// SandboxGateHooks bridges the admission gate's observer seam to a loop's event
// stream (change 0077). It mirrors SandboxEventHooks exactly: the gate reports
// notable admissions in bounded terms (sandbox.AdmissionInfo) and this is where
// those become KindSandboxAdmission events. Keeping the translation here — not in
// internal/tools/sandbox — is what stops the sandbox package from growing a
// dependency on the event vocabulary.
//
// store is the LOOP'S OWN sink, already bound to that loop's StreamKey; nodeID is
// the loop's root node. Both live in the envelope and are never duplicated into
// the payload. A nil store yields inert hooks — the honest shape for a binding
// with no per-loop event store (one-shot, shell, research-probe, mcp-server) —
// so the gate stays live but emits into nothing rather than a NoopStore that
// would make the wiring look active.
//
// NOTE the gate is PROCESS-scoped while the store is per-loop. The composition
// root installs these hooks once per loop via Service.SetGateHooks wherever the
// loop's EventStore is available; a process serving many loops through one
// Service therefore attributes an admission to whichever loop's store was last
// installed. That is an acceptable coarseness for a host-wide capacity signal:
// the tenant is carried on the info and the metric is host-level, so the
// operator-facing rate is correct even when a single event's loop attribution is
// approximate.
func SandboxGateHooks(store event.EventStore, nodeID string) sandbox.GateHooks {
	if store == nil {
		return sandbox.GateHooks{}
	}
	emit := func(outcome string, i sandbox.AdmissionInfo) {
		payload := event.SandboxAdmissionPayload{
			Handler: i.Handler,
			Outcome: outcome,
			Scope:   sandboxAdmissionScope(i.Scope),
		}
		// wait_ms is present only when there was a wait — a refusal is immediate,
		// and a zero would read as an impossibly fast queue in the histogram.
		if i.Waited > 0 {
			payload.WaitMS = i.Waited.Milliseconds()
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			return
		}
		_ = store.Append(event.Event{NodeID: nodeID, Kind: event.KindSandboxAdmission, Payload: raw})
	}
	return sandbox.GateHooks{
		Queued:  func(i sandbox.AdmissionInfo) { emit(event.SandboxAdmissionQueued, i) },
		Refused: func(i sandbox.AdmissionInfo) { emit(event.SandboxAdmissionRefused, i) },
	}
}

// sandboxAdmissionScope maps the gate's closed scope enum onto the wire string.
// An unrecognised scope becomes "" rather than passing through: it ends up as a
// metric label, and an unbounded label is how a closed enum stops being closed.
func sandboxAdmissionScope(s sandbox.AdmissionScope) string {
	switch s {
	case sandbox.ScopeGlobal:
		return "global"
	case sandbox.ScopeTenant:
		return "tenant"
	default:
		return ""
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
