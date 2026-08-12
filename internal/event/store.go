package event

import "context"

// ---- durable seam (change 0047) ----
//
// The durable seam generalizes the legacy EventStore into a tenant-scoped,
// context-carrying contract that can be backed by a process-local filesystem
// (fsstore) or a shared database (pgstore). It is ADDITIVE: the legacy EventStore
// and NoopStore below are untouched and byte-compatible so existing callers keep
// compiling.

// TenantID names an isolation boundary. Every durable stream and registry record
// is scoped to a (tenant, loop) pair; a tenant never observes another tenant's
// stream. The empty tenant normalizes to DefaultTenant.
type TenantID string

// LoopID names a single loop's event stream within a tenant. It is the durable,
// cross-process identity of a loop (formerly the local session/loop id).
type LoopID string

// StreamKey identifies one durable event stream: a (tenant, loop) pair. It is the
// per-loop total-order scope (ADR-0025) and the tenant isolation boundary.
type StreamKey struct {
	Tenant TenantID
	Loop   LoopID
}

// DefaultTenant is the tenant an empty TenantID maps to, so single-tenant local
// behavior (one filesystem tree, no tenant prefix in the caller) is preserved.
const DefaultTenant TenantID = "_default"

// NormalizeTenant maps the empty tenant to DefaultTenant and passes any other
// tenant through unchanged. Backends call this before keying storage so an empty
// caller tenant and DefaultTenant address the same stream.
func NormalizeTenant(t TenantID) TenantID {
	if t == "" {
		return DefaultTenant
	}
	return t
}

// DurableStore is the tenant-scoped, context-carrying generalization of
// EventStore. Unlike the legacy per-session EventStore (one store == one stream),
// a single DurableStore serves many (tenant, loop) streams keyed by StreamKey, so
// one shared backend (a database, or a filesystem tree) can hold every loop's
// history and be reached from any instance.
//
// Invariants preserved from the legacy seam:
//   - The store is the sole Seq allocator; callers pass Seq == 0 (ADR-0025).
//   - Seq is a per-loop total order scoped to the StreamKey (ADR-0025).
//   - Append never blocks on a slow/absent subscriber — non-blocking
//     drop-newest-with-gap fan-out (ADR-0016/ADR-0025).
//   - Events are born plaintext, independent of any segment store (ADR-0024).
type DurableStore interface {
	// Append records one event on the (tenant, loop) stream named by key. The
	// store allocates e.Seq per-loop from 1; callers pass Seq == 0. It MUST NOT
	// block on a slow or absent subscriber. It honors ctx.Err() at entry.
	Append(ctx context.Context, key StreamKey, e Event) error

	// Subscribe returns a live tail of the (tenant, loop) stream plus an
	// idempotent unsubscribe closure. Delivery is non-blocking per subscriber: a
	// full buffer drops the newest event and records a gap rather than
	// back-pressuring Append.
	Subscribe(ctx context.Context, key StreamKey) (<-chan Event, func(), error)

	// Replay returns durable history for the (tenant, loop) stream with Seq > from
	// (from == 0 ⇒ all), in Seq order, tenant-scoped. Subscribe + Replay(from)
	// together are the cross-instance reattach primitive.
	Replay(ctx context.Context, key StreamKey, from Seq) ([]Event, error)
}

// CommittedDurableStore exposes the exact envelope accepted by a durable store.
// It is required by consumers that must observe store-assigned identity, such as
// Seq and a defaulted timestamp, without racing a replay or lossy live tail.
// Append remains available through DurableStore for callers that do not need the
// committed envelope.
type CommittedDurableStore interface {
	DurableStore
	AppendCommitted(ctx context.Context, key StreamKey, e Event) (Event, error)
}

// EventStore is the agent-free seam over the loop event stream. The loop Appends;
// consumers (a TUI, an external binding, the session-log projection) Subscribe for
// a live tail or Replay durable history from a cursor. All three implementations
// (the fs store, the no-op default) satisfy this one interface.
type EventStore interface {
	// Append records one event. It is best-effort and MUST NOT block on a slow or
	// absent subscriber (ADR-0016): the loop calls it inline and a subscriber can
	// never wedge an agent's scheduler slot. The store allocates Seq; callers pass
	// Seq == 0. An error is advisory — the loop logs it and continues.
	Append(Event) error

	// Subscribe returns a live channel tailing events from now, plus an unsubscribe
	// closure (idempotent). Multiple concurrent subscribers are supported. Delivery
	// is non-blocking per subscriber: a full buffer drops the newest event and marks
	// a gap rather than back-pressuring Append.
	Subscribe() (<-chan Event, func())

	// Replay returns durable history with Seq > from (from == 0 ⇒ all), in Seq
	// order. Subscribe + Replay(from) together are the reattach primitive: replay to
	// the last-seen Seq, then tail live.
	Replay(from Seq) ([]Event, error)
}

// NoopStore is the graceful-degradation default installed when no real store is
// wired (one-shot / probe / mcp paths). Every method is inert, so an agent whose
// eventSink is a NoopStore emits into the void without ever nil-panicking —
// mirroring the segment sink's no-op default.
type NoopStore struct{}

func (NoopStore) Append(Event) error { return nil }

func (NoopStore) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event)
	close(ch) // a closed channel yields immediately, so a consumer range ends at once
	return ch, func() {}
}

func (NoopStore) Replay(Seq) ([]Event, error) { return nil, nil }
