package observe

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
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
	// Verdict, DecisionLayer, and ClassifierOutcome are the bounded decision
	// classifications projected from permission.decision events (audit
	// completeness, change 0069 follow-up). They are closed enums — verdict
	// allow|ask|deny, layer a permissions.Layer* constant, classifier outcome
	// ok|truncated|parse_error|cached — never reason text or command previews,
	// so the payload-free contract holds. Empty on every other event kind.
	Verdict           string `json:"verdict,omitempty"`
	DecisionLayer     string `json:"decision_layer,omitempty"`
	ClassifierOutcome string `json:"classifier_outcome,omitempty"`
	// Handler, Runtime, ContainerID, Reason, Cause, Reused, and ColdStartMS are
	// the bounded substrate facts projected from the sandbox lifecycle events
	// (change 0063). Every string is a closed enum — handler container|host|microvm,
	// runtime docker|nerdctl|podman, cause an event.SandboxCause, reason the health
	// enum — except ContainerID, which is the short runtime-assigned id and is
	// bounded by construction. The command, its environment, its output, and any
	// raw error are never projected. Empty on every non-sandbox event kind.
	//
	// Runtime is present only on acquire: the shared release/reap payload and the
	// health payload carry no runtime, and the projection does not invent one
	// rather than emit a label that would aggregate under "".
	//
	// Reused and ColdStartMS come only from sandbox.acquire. Reused is false both
	// for a cold spawn and for every non-acquire event; Operation == OperationSandbox
	// with EventName sandbox.acquire disambiguates the two. ColdStartMS is absent on
	// a warm reuse, where there was no start to measure.
	Handler     string `json:"handler,omitempty"`
	Runtime     string `json:"runtime,omitempty"`
	ContainerID string `json:"container_id,omitempty"`
	Reason      string `json:"reason,omitempty"`
	Cause       string `json:"cause,omitempty"`
	Reused      bool   `json:"reused,omitempty"`
	ColdStartMS int64  `json:"cold_start_ms,omitempty"`
	// TraceID and SpanID preserve only validated correlation identifiers from a
	// committed envelope. The durable carrier (including tracestate) is never
	// retained by the payload-free projection.
	TraceID string `json:"-"`
	SpanID  string `json:"-"`
}

// ProjectEvent extracts only envelope identity and bounded classifications. It
// never retains Event.Payload or Event.Trace; it preserves only the validated
// trace and span identifiers needed to correlate a projected JSON log with its
// originating trace. Tool results decode only their error marker to choose a
// fixed category.
func ProjectEvent(key event.StreamKey, e event.Event) Record {
	op, outcome, category := classify(e.Kind, e.Payload)
	traceID, spanID := correlationIDs(e.Trace)
	rec := Record{Timestamp: e.TS, EventName: string(e.Kind), TenantID: event.NormalizeTenant(key.Tenant), LoopID: key.Loop, NodeID: e.NodeID, Sequence: e.Seq, Operation: op, Outcome: outcome, ErrorCategory: category, TraceID: traceID, SpanID: spanID}
	switch e.Kind {
	case event.KindPermissionDecision:
		decoratePermission(&rec, e.Payload)
	case event.KindSandboxAcquire, event.KindSandboxRelease, event.KindSandboxReap, event.KindSandboxHealth:
		decorateSandbox(&rec, e.Kind, e.Payload)
	}
	return rec
}

// decoratePermission extracts ONLY the bounded enum classifications from a
// permission.decision payload — verdict, deciding layer, and the classifier
// reply's health — mirroring the tool.result error-marker exception. Reason
// text, command previews, and model identity stay excluded from the projection.
func decoratePermission(rec *Record, payload []byte) {
	var marker struct {
		Verdict    string `json:"verdict"`
		Layer      string `json:"layer"`
		Classifier *struct {
			Truncated bool `json:"truncated"`
			ParseOK   bool `json:"parse_ok"`
			Cached    bool `json:"cached"`
		} `json:"classifier"`
	}
	if err := json.Unmarshal(payload, &marker); err != nil {
		return
	}
	rec.Verdict = marker.Verdict
	rec.DecisionLayer = marker.Layer
	if c := marker.Classifier; c != nil {
		switch {
		case c.Cached:
			rec.ClassifierOutcome = "cached"
		case c.Truncated && !c.ParseOK:
			rec.ClassifierOutcome = "truncated"
		case !c.ParseOK:
			rec.ClassifierOutcome = "parse_error"
		default:
			rec.ClassifierOutcome = "ok"
		}
	}
}

// sandboxMarker is the private decode shape for the sandbox lifecycle payloads.
// It names ONLY the bounded fields the projection may keep, so an unbounded
// field added to a payload later (or injected by a hostile writer) is discarded
// by json.Unmarshal rather than needing a separate scrub.
type sandboxMarker struct {
	Handler     string `json:"handler"`
	Runtime     string `json:"runtime"`
	ContainerID string `json:"container_id"`
	Reused      bool   `json:"reused"`
	ColdStartMS int64  `json:"cold_start_ms"`
	Cause       string `json:"cause"`
	Healthy     bool   `json:"healthy"`
	Reason      string `json:"reason"`
}

// healthReasonRecovered is the one health reason that denotes a return to
// service rather than a fault. It mirrors the enum documented on
// event.SandboxHealthPayload.
const healthReasonRecovered = "recovered"

// decorateSandbox lifts ONLY the bounded substrate facts from a sandbox
// lifecycle payload, mirroring decoratePermission. Which facts are meaningful
// depends on the kind, so fields are assigned per kind instead of copied
// wholesale: a stray "reason" on an acquire payload is not a fact about that
// acquire and must not project as one.
func decorateSandbox(rec *Record, kind event.Kind, payload []byte) {
	var marker sandboxMarker
	if err := json.Unmarshal(payload, &marker); err != nil {
		return
	}
	rec.Handler = marker.Handler
	rec.ContainerID = marker.ContainerID
	switch kind {
	case event.KindSandboxAcquire:
		// Acquire is the only payload carrying the runtime and the spawn facts.
		rec.Runtime = marker.Runtime
		rec.Reused = marker.Reused
		rec.ColdStartMS = marker.ColdStartMS
	case event.KindSandboxRelease, event.KindSandboxReap:
		rec.Cause = marker.Cause
	case event.KindSandboxHealth:
		rec.Reason = marker.Reason
	}
}

func correlationIDs(carrier *event.TraceCarrier) (string, string) {
	if carrier == nil || carrier.Validate() != nil {
		return "", ""
	}
	parts := strings.Split(carrier.TraceParent, "-")
	if len(parts) != 4 {
		return "", ""
	}
	return parts[1], parts[2]
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
	case event.KindPermissionDecision:
		// A deny is a decision, not an operational failure: always success.
		// The verdict/layer enums are added by decoratePermission.
		return OperationPermission, OutcomeSuccess, ErrorCategoryNone
	case event.KindSandboxAcquire, event.KindSandboxRelease, event.KindSandboxReap:
		// Checking a context out, handing it back, and the pool reclaiming it are
		// all normal lifecycle traffic. WHY it stopped being held (including the
		// isolation-violating stale_checkout) rides in the Record's bounded Cause
		// enum via decorateSandbox — it is not folded into the outcome, so a
		// reap-rate alert reads the cause label rather than an error count.
		return OperationSandbox, OutcomeSuccess, ErrorCategoryNone
	case event.KindSandboxHealth:
		// Healthy is the only payload bit that decides the outcome, decoded into a
		// private shape so nothing else crosses. A transition INTO health is a
		// success; a transition out of it is the substrate failing under the bash
		// tool, so it takes the tool error category.
		var marker struct {
			Healthy bool   `json:"healthy"`
			Reason  string `json:"reason"`
		}
		_ = json.Unmarshal(payload, &marker)
		if marker.Healthy || marker.Reason == healthReasonRecovered {
			return OperationSandbox, OutcomeSuccess, ErrorCategoryNone
		}
		return OperationSandbox, OutcomeError, ErrorCategoryTool
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
