package observe

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/event"
)

func TestProjectEventIsPayloadFreeForEveryKind(t *testing.T) {
	t.Parallel()
	payload, err := json.Marshal(map[string]any{
		"prompt": "secret prompt", "response": "secret response", "delta": "secret delta",
		"args": map[string]string{"token": "secret"}, "result": "secret result",
		"authorization": "Bearer secret", "traceparent": "00-secret", "err": "raw secret error",
	})
	if err != nil {
		t.Fatal(err)
	}
	kinds := []event.Kind{event.KindTurnStart, event.KindTurnEnd, event.KindModelCallStart, event.KindModelDelta, event.KindModelCallEnd, event.KindToolCall, event.KindToolResult, event.KindSpawnStart, event.KindSpawnDone, event.KindSummarize, event.KindLoopTrip, event.KindError, event.KindLoopParked}
	for _, kind := range kinds {
		t.Run(string(kind), func(t *testing.T) {
			record := ProjectEvent(event.StreamKey{Tenant: "acme", Loop: "same-loop"}, event.Event{Seq: 7, TS: time.Date(2026, 8, 12, 1, 2, 3, 0, time.UTC), NodeID: "node-1", Kind: kind, Payload: payload, Trace: &event.TraceCarrier{TraceParent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"}})
			if record.Timestamp.IsZero() || record.EventName != string(kind) || record.TenantID != "acme" || record.LoopID != "same-loop" || record.NodeID != "node-1" || record.Sequence != 7 {
				t.Fatalf("identity fields missing: %#v", record)
			}
			if record.Operation == "" || record.Outcome == "" || record.ErrorCategory == "" {
				t.Fatalf("bounded fields missing: %#v", record)
			}
			encoded, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{"secret prompt", "secret response", "secret delta", "secret result", "Bearer", "traceparent", "raw secret error"} {
				if strings.Contains(string(encoded), secret) {
					t.Fatalf("projection leaked %q: %s", secret, encoded)
				}
			}
		})
	}
}

func TestProjectEventClassifiesToolFailureWithoutRawError(t *testing.T) {
	payload, err := event.MarshalPayload(event.ToolResultPayload{IsError: true, Result: "credential leaked"})
	if err != nil {
		t.Fatal(err)
	}
	record := ProjectEvent(event.StreamKey{Tenant: "acme", Loop: "l"}, event.Event{Kind: event.KindToolResult, Payload: payload})
	if record.Outcome != OutcomeError || record.ErrorCategory != ErrorCategoryTool {
		t.Fatalf("got %#v", record)
	}
}

func TestProjectEventCarriesOnlyValidatedTraceIDs(t *testing.T) {
	key := event.StreamKey{Tenant: "acme", Loop: "same-loop"}
	withTrace := ProjectEvent(key, event.Event{Trace: &event.TraceCarrier{
		TraceParent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		TraceState:  "acme=tenant-sensitive-state",
	}})
	if withTrace.TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" || withTrace.SpanID != "00f067aa0ba902b7" {
		t.Fatalf("correlation IDs = %q, %q", withTrace.TraceID, withTrace.SpanID)
	}
	encoded, err := json.Marshal(withTrace)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "tenant-sensitive-state") {
		t.Fatalf("projection leaked tracestate: %s", encoded)
	}
	if got := ProjectEvent(key, event.Event{Trace: &event.TraceCarrier{TraceParent: "bad"}}); got.TraceID != "" || got.SpanID != "" {
		t.Fatalf("invalid carrier propagated identifiers: %#v", got)
	}
}

func TestFanoutReportsAllProjectorFailures(t *testing.T) {
	var calls int
	fanout := Fanout{ProjectorFunc(func(context.Context, Record) error { calls++; return nil }), ProjectorFunc(func(context.Context, Record) error { calls++; return assertError{} })}
	if err := fanout.Project(context.Background(), Record{}); err == nil || calls != 2 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

type assertError struct{}

func (assertError) Error() string { return "test failure" }

func TestProjectEventLoopCancelledIsBoundedAndPayloadFree(t *testing.T) {
	t.Parallel()
	key := event.StreamKey{Tenant: "acme", Loop: "loop-1"}
	for _, tc := range []struct {
		wasLive bool
		want    string
	}{{true, "true"}, {false, "false"}} {
		payload, err := json.Marshal(event.LoopCancelledPayload{Reason: "secret reason", RequestedBy: "secret-principal", WasLive: tc.wasLive, RequestSeq: 9})
		if err != nil {
			t.Fatal(err)
		}
		rec := ProjectEvent(key, event.Event{Seq: 12, Kind: event.KindLoopCancelled, Payload: payload})
		if rec.Operation != OperationLoop || rec.Outcome != OutcomeCanceled || rec.ErrorCategory != ErrorCategoryNone {
			t.Fatalf("was_live=%v classified as %s/%s/%s", tc.wasLive, rec.Operation, rec.Outcome, rec.ErrorCategory)
		}
		if rec.CancelWasLive != tc.want {
			t.Fatalf("CancelWasLive = %q, want %q", rec.CancelWasLive, tc.want)
		}
		encoded, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		for _, leaked := range []string{"secret reason", "secret-principal", "requested_by"} {
			if strings.Contains(string(encoded), leaked) {
				t.Fatalf("projection leaked %q: %s", leaked, encoded)
			}
		}
	}

	// A payload that does not decode leaves CancelWasLive empty.
	if rec := ProjectEvent(key, event.Event{Kind: event.KindLoopCancelled, Payload: []byte("{not json")}); rec.CancelWasLive != "" {
		t.Fatalf("undecodable payload CancelWasLive = %q, want empty", rec.CancelWasLive)
	}

	reqPayload, err := json.Marshal(event.LoopCancelRequestPayload{Reason: "secret reason", RequestedBy: "secret-principal"})
	if err != nil {
		t.Fatal(err)
	}
	req := ProjectEvent(key, event.Event{Kind: event.KindLoopCancelRequest, Payload: reqPayload})
	steer := ProjectEvent(key, event.Event{Kind: event.KindSteerRequest, Payload: reqPayload})
	if req.Operation != steer.Operation || req.Outcome != steer.Outcome || req.ErrorCategory != steer.ErrorCategory {
		t.Fatalf("loop.cancel.request = %s/%s/%s, steer.request = %s/%s/%s", req.Operation, req.Outcome, req.ErrorCategory, steer.Operation, steer.Outcome, steer.ErrorCategory)
	}
	if req.CancelWasLive != "" {
		t.Fatalf("loop.cancel.request CancelWasLive = %q, want empty", req.CancelWasLive)
	}

	// Every other kind keeps CancelWasLive empty, even with a was_live field in its payload.
	wasLive := []byte(`{"was_live":true}`)
	for _, kind := range []event.Kind{event.KindTurnStart, event.KindTurnEnd, event.KindToolResult, event.KindLoopParked, event.KindError, event.KindSpawnDone} {
		if rec := ProjectEvent(key, event.Event{Kind: kind, Payload: wasLive}); rec.CancelWasLive != "" {
			t.Fatalf("%s CancelWasLive = %q, want empty", kind, rec.CancelWasLive)
		}
	}
}
