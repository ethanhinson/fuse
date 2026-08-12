// Package observe defines provider-neutral observation contracts for Fuse.
// Concrete OpenTelemetry, Prometheus, and logging adapters belong in child
// packages so importing this package never pulls a telemetry vendor into core
// loop packages.
package observe

import "context"

// OperationKind identifies the bounded class of work being observed. It is a
// repository-owned vocabulary: adapters must not use it to pass provider names
// or unbounded request data.
type OperationKind string

const (
	OperationAPIRequest   OperationKind = "api.request"
	OperationLoop         OperationKind = "loop"
	OperationStore        OperationKind = "store"
	OperationModelAttempt OperationKind = "model.attempt"
	OperationTool         OperationKind = "tool"
	OperationSpawn        OperationKind = "spawn"
	OperationPubSub       OperationKind = "pubsub"
)

// Outcome is the bounded terminal result of an operation.
type Outcome string

const (
	OutcomeSuccess  Outcome = "success"
	OutcomeError    Outcome = "error"
	OutcomeTimeout  Outcome = "timeout"
	OutcomeCanceled Outcome = "canceled"
)

// Field is a bounded, already-sanitized observation attribute. Values are
// strings deliberately: callers must classify or redact data before it reaches
// routine telemetry instead of accidentally passing payloads or errors through
// arbitrary values.
type Field struct {
	Key   string
	Value string
}

// Descriptor identifies an operation at its start. Name and Kind must be from
// repository-owned, bounded vocabularies; Fields carry only safe attributes.
type Descriptor struct {
	Kind   OperationKind
	Name   string
	Fields []Field
}

// Observer starts an operation and returns the context downstream work should
// receive plus a handle that must be ended once. Implementations must be safe
// for concurrent use by loop execution.
type Observer interface {
	Start(context.Context, Descriptor) (context.Context, Handle)
}

// Handle completes an operation. Extra fields are terminal safe attributes.
type Handle interface {
	End(Outcome, ...Field)
}

// NoopObserver disables observation without changing context propagation or
// requiring a telemetry backend.
type NoopObserver struct{}

// Start returns ctx unchanged and an inert handle.
func (NoopObserver) Start(ctx context.Context, _ Descriptor) (context.Context, Handle) {
	return ctx, NoopHandle{}
}

// NoopHandle safely absorbs all terminal signals.
type NoopHandle struct{}

// End implements Handle.
func (NoopHandle) End(Outcome, ...Field) {}
