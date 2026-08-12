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

func TestFanoutReportsAllProjectorFailures(t *testing.T) {
	var calls int
	fanout := Fanout{ProjectorFunc(func(context.Context, Record) error { calls++; return nil }), ProjectorFunc(func(context.Context, Record) error { calls++; return assertError{} })}
	if err := fanout.Project(context.Background(), Record{}); err == nil || calls != 2 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

type assertError struct{}

func (assertError) Error() string { return "test failure" }
