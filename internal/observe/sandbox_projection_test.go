package observe

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/event"
)

// Covers the sandbox lifecycle projection (change 0063): the bounded substrate
// identity and closed-enum classifications cross into the Record, and the
// (Tenant, Loop, Node) envelope identity is preserved so a sandbox.acquire joins
// to the tool.call events that ran inside that container.

func sandboxEvent(t *testing.T, kind event.Kind, payload any) event.Event {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return event.Event{TS: time.Now(), Kind: kind, NodeID: "n1", Seq: 7, Payload: raw}
}

func TestProjectSandboxKindsClassify(t *testing.T) {
	key := event.StreamKey{Tenant: "acme", Loop: "loop-1"}
	cases := []struct {
		name    string
		event   event.Event
		outcome Outcome
		errCat  ErrorCategory
	}{
		{
			name:    "acquire",
			event:   sandboxEvent(t, event.KindSandboxAcquire, event.SandboxAcquirePayload{Handler: "container", Runtime: "docker", ContainerID: "abc123", Reused: true}),
			outcome: OutcomeSuccess, errCat: ErrorCategoryNone,
		},
		{
			name:    "release",
			event:   sandboxEvent(t, event.KindSandboxRelease, event.SandboxReleasePayload{Handler: "container", ContainerID: "abc123", Cause: event.SandboxCauseReleased}),
			outcome: OutcomeSuccess, errCat: ErrorCategoryNone,
		},
		{
			name:    "reap",
			event:   sandboxEvent(t, event.KindSandboxReap, event.SandboxReleasePayload{Handler: "container", ContainerID: "abc123", Cause: event.SandboxCauseIdleTTL}),
			outcome: OutcomeSuccess, errCat: ErrorCategoryNone,
		},
		{
			name:    "health unhealthy",
			event:   sandboxEvent(t, event.KindSandboxHealth, event.SandboxHealthPayload{Handler: "container", ContainerID: "abc123", Healthy: false, Reason: "oom"}),
			outcome: OutcomeError, errCat: ErrorCategoryTool,
		},
		{
			name:    "health recovered",
			event:   sandboxEvent(t, event.KindSandboxHealth, event.SandboxHealthPayload{Handler: "container", ContainerID: "abc123", Healthy: true, Reason: "recovered"}),
			outcome: OutcomeSuccess, errCat: ErrorCategoryNone,
		},
		{
			// Defensive: a healthy=true transition with any other reason is still
			// not a failure. Health is decided by the Healthy bit first.
			name:    "healthy without recovered reason",
			event:   sandboxEvent(t, event.KindSandboxHealth, event.SandboxHealthPayload{Handler: "container", Healthy: true}),
			outcome: OutcomeSuccess, errCat: ErrorCategoryNone,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := ProjectEvent(key, tc.event)
			if rec.Operation != OperationSandbox {
				t.Fatalf("operation = %q, want %q", rec.Operation, OperationSandbox)
			}
			if rec.Outcome != tc.outcome {
				t.Fatalf("outcome = %q, want %q", rec.Outcome, tc.outcome)
			}
			if rec.ErrorCategory != tc.errCat {
				t.Fatalf("error category = %q, want %q", rec.ErrorCategory, tc.errCat)
			}
		})
	}
}

func TestProjectSandboxAcquireLiftsBoundedFields(t *testing.T) {
	key := event.StreamKey{Tenant: "acme", Loop: "loop-1"}
	rec := ProjectEvent(key, sandboxEvent(t, event.KindSandboxAcquire, event.SandboxAcquirePayload{
		Handler: "container", Runtime: "docker", ContainerID: "abc123", Reused: false, ColdStartMS: 412,
	}))
	if rec.Handler != "container" || rec.Runtime != "docker" || rec.ContainerID != "abc123" {
		t.Fatalf("substrate identity not lifted: %+v", rec)
	}
	if rec.Reused {
		t.Fatalf("cold spawn must project reused=false, got %+v", rec)
	}
	if rec.ColdStartMS != 412 {
		t.Fatalf("cold-start = %d, want 412", rec.ColdStartMS)
	}
	if rec.Cause != "" || rec.Reason != "" {
		t.Fatalf("acquire carries no cause/reason, got %q/%q", rec.Cause, rec.Reason)
	}
}

func TestProjectSandboxPooledReuse(t *testing.T) {
	key := event.StreamKey{Tenant: "acme", Loop: "loop-1"}
	rec := ProjectEvent(key, sandboxEvent(t, event.KindSandboxAcquire, event.SandboxAcquirePayload{
		Handler: "container", Runtime: "docker", ContainerID: "abc123", Reused: true,
	}))
	if !rec.Reused {
		t.Fatalf("pooled reuse must project reused=true, got %+v", rec)
	}
	if rec.ColdStartMS != 0 {
		t.Fatalf("a warm reuse has no cold start to report, got %d", rec.ColdStartMS)
	}
}

func TestProjectSandboxReleaseAndReapLiftCause(t *testing.T) {
	key := event.StreamKey{Tenant: "acme", Loop: "loop-1"}
	for _, tc := range []struct {
		kind  event.Kind
		cause event.SandboxCause
	}{
		{event.KindSandboxRelease, event.SandboxCauseEarlyReturn},
		{event.KindSandboxRelease, event.SandboxCauseLoopEnd},
		{event.KindSandboxReap, event.SandboxCauseIdleTTL},
		{event.KindSandboxReap, event.SandboxCauseStaleCheckout},
	} {
		rec := ProjectEvent(key, sandboxEvent(t, tc.kind, event.SandboxReleasePayload{
			Handler: "container", ContainerID: "abc123", Cause: tc.cause,
		}))
		if rec.Cause != string(tc.cause) {
			t.Errorf("%s cause = %q, want %q", tc.kind, rec.Cause, tc.cause)
		}
		if rec.Handler != "container" || rec.ContainerID != "abc123" {
			t.Errorf("%s substrate identity not lifted: %+v", tc.kind, rec)
		}
		// The shared release payload carries no runtime; the projection must not
		// invent one, so the label is simply absent here.
		if rec.Runtime != "" {
			t.Errorf("%s must not synthesize a runtime label, got %q", tc.kind, rec.Runtime)
		}
	}
}

func TestProjectSandboxHealthLiftsReason(t *testing.T) {
	key := event.StreamKey{Tenant: "acme", Loop: "loop-1"}
	rec := ProjectEvent(key, sandboxEvent(t, event.KindSandboxHealth, event.SandboxHealthPayload{
		Handler: "container", ContainerID: "abc123", Healthy: false, Reason: "runtime_exit",
	}))
	if rec.Reason != "runtime_exit" {
		t.Fatalf("reason = %q, want runtime_exit", rec.Reason)
	}
	if rec.Handler != "container" || rec.ContainerID != "abc123" {
		t.Fatalf("substrate identity not lifted: %+v", rec)
	}
	if rec.Cause != "" {
		t.Fatalf("health carries no cause, got %q", rec.Cause)
	}
}

// The payload-free contract: the Record retains no raw payload. Even when a
// payload arrives carrying fields the projection does not know — an unbounded
// command, env, or error text that must never reach durable telemetry — nothing
// beyond the bounded set crosses into the marshalled Record.
func TestProjectSandboxRetainsNoRawPayload(t *testing.T) {
	key := event.StreamKey{Tenant: "acme", Loop: "loop-1"}
	hostile := []byte(`{"handler":"container","runtime":"docker","container_id":"abc123",` +
		`"cause":"idle_ttl","reason":"oom","command":"SECRET command","env":{"TOKEN":"SECRET value"},` +
		`"error":"SECRET error text"}`)
	for _, kind := range []event.Kind{event.KindSandboxAcquire, event.KindSandboxRelease, event.KindSandboxReap, event.KindSandboxHealth} {
		rec := ProjectEvent(key, event.Event{TS: time.Now(), Kind: kind, NodeID: "n1", Seq: 7, Payload: hostile})
		raw, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		if s := string(raw); strings.Contains(s, "SECRET") || strings.Contains(s, "TOKEN") {
			t.Fatalf("%s projection leaked payload text: %s", kind, s)
		}
	}
}

func TestNonSandboxEventsCarryNoSandboxFields(t *testing.T) {
	key := event.StreamKey{Tenant: "acme", Loop: "loop-1"}
	rec := ProjectEvent(key, event.Event{TS: time.Now(), Kind: event.KindToolCall,
		Payload: []byte(`{"handler":"container","runtime":"docker","cause":"idle_ttl","reused":true,"cold_start_ms":9}`)})
	if rec.Handler != "" || rec.Runtime != "" || rec.Reason != "" || rec.Cause != "" || rec.ContainerID != "" || rec.Reused || rec.ColdStartMS != 0 {
		t.Fatalf("non-sandbox events must not carry sandbox fields, got %+v", rec)
	}
}

// The join the "which loop is running where" dashboard depends on: a
// sandbox.acquire and the tool.call events that follow it in the same loop share
// the (Tenant, Loop, Node) envelope identity, so a container id can be attributed
// to the work that ran inside it. Correlation lives in the envelope, never in the
// payload.
func TestSandboxAcquireCorrelatesWithToolCallsInSameLoop(t *testing.T) {
	key := event.StreamKey{Tenant: "acme", Loop: "loop-1"}
	acquire := ProjectEvent(key, event.Event{
		TS: time.Now(), Kind: event.KindSandboxAcquire, NodeID: "node-7", Seq: 3,
		Payload: mustJSON(t, event.SandboxAcquirePayload{Handler: "container", Runtime: "docker", ContainerID: "abc123"}),
	})
	tool := ProjectEvent(key, event.Event{
		TS: time.Now(), Kind: event.KindToolCall, NodeID: "node-7", Seq: 4,
		Payload: []byte(`{"name":"bash"}`),
	})

	if acquire.TenantID != tool.TenantID || acquire.LoopID != tool.LoopID || acquire.NodeID != tool.NodeID {
		t.Fatalf("acquire %v/%v/%v must share identity with tool.call %v/%v/%v",
			acquire.TenantID, acquire.LoopID, acquire.NodeID, tool.TenantID, tool.LoopID, tool.NodeID)
	}
	if acquire.ContainerID == "" {
		t.Fatal("acquire must carry the container id the join resolves to")
	}
	if acquire.NodeID == "" || acquire.LoopID == "" || acquire.TenantID == "" {
		t.Fatalf("the join keys must all be populated, got %+v", acquire)
	}
	// A different loop must not join to this container.
	other := ProjectEvent(event.StreamKey{Tenant: "acme", Loop: "loop-2"}, event.Event{
		TS: time.Now(), Kind: event.KindToolCall, NodeID: "node-7", Seq: 1,
	})
	if other.LoopID == acquire.LoopID {
		t.Fatal("a distinct loop must not share the acquire's loop id")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
