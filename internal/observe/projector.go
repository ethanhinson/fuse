package observe

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/ethanhinson/fuse/internal/event"
)

// ErrorCategory is a bounded classification suitable for routine telemetry.
// It never carries a raw error message or event payload.
type ErrorCategory string

const (
	ErrorCategoryNone       ErrorCategory = "none"
	ErrorCategoryTool       ErrorCategory = "tool_error"
	ErrorCategoryLoop       ErrorCategory = "loop_error"
	ErrorCategoryProjection ErrorCategory = "projection_failure"
)

// Record is the payload-free, tenant-scoped projection of an event.
type Record struct {
	Timestamp     time.Time      `json:"timestamp"`
	EventName     string         `json:"event_name"`
	TenantID      event.TenantID `json:"tenant_id"`
	LoopID        event.LoopID   `json:"loop_id"`
	NodeID        string         `json:"node_id"`
	Sequence      event.Seq      `json:"sequence"`
	Operation     OperationKind  `json:"operation"`
	Outcome       Outcome        `json:"outcome"`
	ErrorCategory ErrorCategory  `json:"error_category"`
}

// ProjectEvent extracts only envelope identity and bounded classifications. It
// never retains Event.Payload or Event.Trace; tool results decode only their
// error marker to choose a fixed category.
func ProjectEvent(key event.StreamKey, e event.Event) Record {
	op, outcome, category := classify(e.Kind, e.Payload)
	return Record{Timestamp: e.TS, EventName: string(e.Kind), TenantID: event.NormalizeTenant(key.Tenant), LoopID: key.Loop, NodeID: e.NodeID, Sequence: e.Seq, Operation: op, Outcome: outcome, ErrorCategory: category}
}

func classify(kind event.Kind, payload []byte) (OperationKind, Outcome, ErrorCategory) {
	switch kind {
	case event.KindModelCallStart, event.KindModelDelta, event.KindModelCallEnd:
		return OperationModelAttempt, OutcomeSuccess, ErrorCategoryNone
	case event.KindToolCall:
		return OperationTool, OutcomeSuccess, ErrorCategoryNone
	case event.KindToolResult:
		// IsError is the only payload bit allowed into the projection. Decode into a
		// private shape so result text, arguments, and provider errors stay excluded.
		var marker struct {
			IsError bool `json:"is_error"`
		}
		_ = json.Unmarshal(payload, &marker)
		if marker.IsError {
			return OperationTool, OutcomeError, ErrorCategoryTool
		}
		return OperationTool, OutcomeSuccess, ErrorCategoryNone
	case event.KindSpawnStart, event.KindSpawnDone:
		return OperationSpawn, OutcomeSuccess, ErrorCategoryNone
	case event.KindError, event.KindLoopTrip:
		return OperationLoop, OutcomeError, ErrorCategoryLoop
	default:
		return OperationLoop, OutcomeSuccess, ErrorCategoryNone
	}
}

// Projector consumes a safe projection. Implementations must not feed errors
// back into loop execution; Runner treats failures as best-effort telemetry.
type Projector interface {
	Project(context.Context, Record) error
}
type ProjectorFunc func(context.Context, Record) error

func (f ProjectorFunc) Project(ctx context.Context, record Record) error { return f(ctx, record) }

// Fanout attempts every projector even when one fails.
type Fanout []Projector

func (f Fanout) Project(ctx context.Context, record Record) error {
	var errs []error
	for _, projector := range f {
		if projector != nil {
			if err := projector.Project(ctx, record); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}
