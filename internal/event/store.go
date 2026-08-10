package event

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
