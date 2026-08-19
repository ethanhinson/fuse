package observe

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/event"
)

// Covers the permission.decision projection (audit completeness, 0069
// follow-up): the bounded verdict/layer/classifier-health enums cross into the
// Record; reason text and command previews do not.

func permissionEvent(t *testing.T, payload event.PermissionDecisionPayload) event.Event {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return event.Event{TS: time.Now(), Kind: event.KindPermissionDecision, NodeID: "n1", Seq: 7, Payload: raw}
}

func TestProjectPermissionDecision(t *testing.T) {
	key := event.StreamKey{Tenant: "acme", Loop: "loop-1"}
	rec := ProjectEvent(key, permissionEvent(t, event.PermissionDecisionPayload{
		Tool: "bash", Verdict: "deny", Layer: "classifier",
		Reason: "SECRET rationale must not project", Command: "SECRET command must not project",
		Classifier: &event.ClassifierCallPayload{Model: "cloud/x", ParseOK: true},
	}))

	if rec.Operation != OperationPermission || rec.Outcome != OutcomeSuccess {
		t.Fatalf("decision must classify as permission/success, got %s/%s", rec.Operation, rec.Outcome)
	}
	if rec.Verdict != "deny" || rec.DecisionLayer != "classifier" {
		t.Fatalf("verdict/layer enums must project, got %q/%q", rec.Verdict, rec.DecisionLayer)
	}
	if rec.ClassifierOutcome != "ok" {
		t.Fatalf("clean classifier reply projects outcome ok, got %q", rec.ClassifierOutcome)
	}
	// The payload-free contract: no free text crosses into the projection.
	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if s := string(raw); strings.Contains(s, "SECRET") || strings.Contains(s, "cloud/x") {
		t.Fatalf("projection leaked payload text: %s", s)
	}
}

func TestProjectPermissionClassifierHealth(t *testing.T) {
	key := event.StreamKey{Tenant: "acme", Loop: "loop-1"}
	cases := []struct {
		cls  *event.ClassifierCallPayload
		want string
	}{
		{nil, ""},
		{&event.ClassifierCallPayload{ParseOK: true}, "ok"},
		{&event.ClassifierCallPayload{ParseOK: true, Cached: true}, "cached"},
		{&event.ClassifierCallPayload{Truncated: true}, "truncated"},
		{&event.ClassifierCallPayload{}, "parse_error"},
	}
	for _, tc := range cases {
		rec := ProjectEvent(key, permissionEvent(t, event.PermissionDecisionPayload{
			Tool: "bash", Verdict: "ask", Layer: "classifier", Classifier: tc.cls,
		}))
		if rec.ClassifierOutcome != tc.want {
			t.Errorf("classifier %+v → outcome %q, want %q", tc.cls, rec.ClassifierOutcome, tc.want)
		}
	}
}

func TestNonPermissionEventsCarryNoDecisionFields(t *testing.T) {
	key := event.StreamKey{Tenant: "acme", Loop: "loop-1"}
	rec := ProjectEvent(key, event.Event{TS: time.Now(), Kind: event.KindToolCall, Payload: []byte(`{"verdict":"allow","layer":"x"}`)})
	if rec.Verdict != "" || rec.DecisionLayer != "" || rec.ClassifierOutcome != "" {
		t.Fatalf("non-decision events must not carry decision fields, got %+v", rec)
	}
}
