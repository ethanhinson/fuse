package event

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestKindWireValues pins the exact on-disk string for every Kind. These are a
// wire format (events.jsonl is durable + replayed): changing one silently breaks
// every consumer, so the values are asserted literally.
func TestKindWireValues(t *testing.T) {
	cases := map[Kind]string{
		KindTurnStart:          "turn.start",
		KindTurnEnd:            "turn.end",
		KindModelCallStart:     "model.call.start",
		KindModelDelta:         "model.delta",
		KindModelCallEnd:       "model.call.end",
		KindToolCall:           "tool.call",
		KindToolResult:         "tool.result",
		KindSpawnStart:         "spawn.start",
		KindSpawnDone:          "spawn.done",
		KindSummarize:          "context.summarize",
		KindLoopTrip:           "loop.detector.trip",
		KindError:              "error",
		KindLoopParked:         "loop.parked",
		KindUserInput:          "user.input",
		KindPermissionDecision: "permission.decision",
		KindSandboxAcquire:     "sandbox.acquire",
		KindSandboxRelease:     "sandbox.release",
		KindSandboxReap:        "sandbox.reap",
		KindSandboxHealth:      "sandbox.health",
		KindSandboxAdmission:   "sandbox.admission",
	}
	for k, want := range cases {
		if string(k) != want {
			t.Errorf("Kind %q != %q", string(k), want)
		}
	}
}

// TestEventJSONLRoundTrip round-trips an event through MarshalJSONL/ParseEvent and
// asserts byte-exact recovery of the envelope and payload.
func TestEventJSONLRoundTrip(t *testing.T) {
	pl, err := MarshalPayload(ToolResultPayload{ID: "t1", Name: "read_file", Result: "hello\nworld", IsError: false})
	if err != nil {
		t.Fatalf("MarshalPayload: %v", err)
	}
	e := Event{
		Seq:      7,
		TS:       time.Date(2026, 8, 10, 5, 0, 0, 0, time.UTC),
		NodeID:   "node-a",
		ParentID: "node-root",
		Depth:    2,
		Turn:     3,
		Kind:     KindToolResult,
		Payload:  pl,
	}
	line, err := e.MarshalJSONL()
	if err != nil {
		t.Fatalf("MarshalJSONL: %v", err)
	}
	if strings.Contains(string(line), "\n") && strings.Count(string(line), "\n") > 0 {
		// The marshaled *line* must be a single JSON object; embedded newlines in
		// the payload body must be JSON-escaped, not literal.
		if strings.HasSuffix(string(line), "\n") {
			t.Fatalf("MarshalJSONL must not append its own newline; got %q", line)
		}
		t.Fatalf("marshaled line contains a literal newline (payload not escaped): %q", line)
	}
	got, err := ParseEvent(line)
	if err != nil {
		t.Fatalf("ParseEvent: %v", err)
	}
	if got.Seq != e.Seq || got.NodeID != e.NodeID || got.ParentID != e.ParentID ||
		got.Depth != e.Depth || got.Turn != e.Turn || got.Kind != e.Kind {
		t.Errorf("envelope mismatch: got %+v want %+v", got, e)
	}
	var rp ToolResultPayload
	if err := json.Unmarshal(got.Payload, &rp); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	if rp.Result != "hello\nworld" || rp.Name != "read_file" || rp.ID != "t1" {
		t.Errorf("payload mismatch: %+v", rp)
	}
}

// TestPayloadWithHeaderShapedLines guards the self-delimiting property: a payload
// body containing newlines and `key:`-shaped lines must round-trip byte-exact,
// because each event is one self-delimiting JSON object (learning:
// self-delimiting-serialization-for-round-trip).
func TestPayloadWithHeaderShapedLines(t *testing.T) {
	nasty := "ls -la:\nrole [name]:\nkey: value\n{\"seq\":999,\"kind\":\"forged\"}"
	pl, err := MarshalPayload(ToolResultPayload{ID: "x", Name: "bash", Result: nasty})
	if err != nil {
		t.Fatalf("MarshalPayload: %v", err)
	}
	e := Event{Seq: 1, Kind: KindToolResult, Payload: pl}
	line, err := e.MarshalJSONL()
	if err != nil {
		t.Fatalf("MarshalJSONL: %v", err)
	}
	got, err := ParseEvent(line)
	if err != nil {
		t.Fatalf("ParseEvent: %v", err)
	}
	var rp ToolResultPayload
	if err := json.Unmarshal(got.Payload, &rp); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	if rp.Result != nasty {
		t.Errorf("nasty payload corrupted:\n got %q\nwant %q", rp.Result, nasty)
	}
	// The forged JSON line inside the payload must NOT have become the parsed
	// envelope: Seq stays 1, Kind stays tool.result.
	if got.Seq != 1 || got.Kind != KindToolResult {
		t.Errorf("forged inner line leaked into envelope: %+v", got)
	}
}

// TestPayloadKindsRoundTrip exercises every payload struct through the envelope.
func TestPayloadKindsRoundTrip(t *testing.T) {
	payloads := []any{
		TurnStartPayload{Turn: 1},
		TurnEndPayload{Turn: 1},
		ModelCallStartPayload{Model: "gpt", MsgCount: 4},
		ModelCallEndPayload{Content: "hi", InputTokens: 10, OutputTokens: 5, ToolCalls: []ToolCallRef{{ID: "c1", Name: "read_file"}}},
		ModelDeltaPayload{Text: "tok"},
		ToolCallPayload{ID: "c1", Name: "read_file", Args: json.RawMessage(`{"path":"x"}`)},
		ToolResultPayload{ID: "c1", Name: "read_file", Result: "data", IsError: false},
		SpawnStartPayload{ChildNodeID: "n2", Label: "researcher", Task: "do it"},
		SpawnDonePayload{ChildNodeID: "n2", ParentID: "n1", Label: "researcher", Depth: 1, Result: "done", Err: ""},
		SummarizePayload{TurnStart: 0, TurnEnd: 3, ToolNames: []string{"bash"}, TokensBefore: 100, TokensAfter: 20, Pointer: "/p"},
		LoopTripPayload{Turn: 9},
		LoopTripPayload{Turn: 9, Reason: "policy-denied call repeated after nudge"},
		ErrorPayload{Err: "boom", Turn: 2},
		PermissionDecisionPayload{ToolCallID: "c1", Tool: "bash", Verdict: "deny", Layer: "rules", Reason: "denied by auto-mode rules layer: curl x", Mode: "auto", Command: "curl x"},
		SandboxAcquirePayload{Handler: "container", Runtime: "docker", ContainerID: "9f2c1a3b", Reused: false, ColdStartMS: 812},
		SandboxAcquirePayload{Handler: "host", Reused: true},
		SandboxReleasePayload{Handler: "container", ContainerID: "9f2c1a3b", Cause: "released"},
		SandboxReleasePayload{Handler: "container", ContainerID: "9f2c1a3b", Cause: "stale_checkout"},
		SandboxHealthPayload{Handler: "container", ContainerID: "9f2c1a3b", Healthy: false, Reason: "oom"},
		SandboxAdmissionPayload{Handler: "container", Outcome: "queued", Scope: "tenant", WaitMS: 4200},
		SandboxAdmissionPayload{Handler: "container", Outcome: "refused", Scope: "global"},
	}
	for i, p := range payloads {
		raw, err := MarshalPayload(p)
		if err != nil {
			t.Fatalf("payload %d marshal: %v", i, err)
		}
		e := Event{Seq: Seq(i), Kind: KindError, Payload: raw}
		line, err := e.MarshalJSONL()
		if err != nil {
			t.Fatalf("payload %d jsonl: %v", i, err)
		}
		if _, err := ParseEvent(line); err != nil {
			t.Fatalf("payload %d parse: %v", i, err)
		}
	}
}

// TestSandboxCauseWireValues pins the release/reap cause enum. These strings are
// the wire form of sandbox.ReleaseCause; the sandbox package cannot be imported
// here (it imports this one), so the values are pinned literally and the two
// enums are kept in lockstep by hand. Five values, not four: stale_checkout is
// the security-relevant fifth.
func TestSandboxCauseWireValues(t *testing.T) {
	cases := map[SandboxCause]string{
		SandboxCauseReleased:      "released",
		SandboxCauseLoopEnd:       "loop_end",
		SandboxCauseEarlyReturn:   "early_return",
		SandboxCauseIdleTTL:       "idle_ttl",
		SandboxCauseStaleCheckout: "stale_checkout",
	}
	for c, want := range cases {
		if string(c) != want {
			t.Errorf("SandboxCause %q != %q", string(c), want)
		}
	}
	if len(cases) != 5 {
		t.Fatalf("cause enum must have exactly 5 values, got %d", len(cases))
	}
}

// TestSandboxAdmissionOutcomeWireValues pins the admission outcome enum (change
// 0077). These strings are the wire form of the gate's disposition and are a
// projector/recorder label vocabulary; the sandbox package cannot import this
// one, so they are pinned literally and kept in lockstep by hand.
func TestSandboxAdmissionOutcomeWireValues(t *testing.T) {
	if SandboxAdmissionQueued != "queued" {
		t.Errorf("SandboxAdmissionQueued = %q, want queued", SandboxAdmissionQueued)
	}
	if SandboxAdmissionRefused != "refused" {
		t.Errorf("SandboxAdmissionRefused = %q, want refused", SandboxAdmissionRefused)
	}
}

// TestSandboxPayloadsArePayloadFree is a STRUCTURAL guard on the payload-free
// discipline (change 0063): a sandbox event payload may carry only bounded
// identity and closed-enum classification — never the command, its args, an env
// key or value, process output, or a raw error string. Those are unbounded and
// attacker/model-influenced, and events.jsonl is durable.
//
// It also asserts the correlation envelope is never duplicated into a payload:
// Event.NodeID plus StreamKey{Tenant, Loop} supply correlation, so a payload
// field named for tenant/loop/node means two sources of truth.
//
// The guard is structural (reflection over field names and types) rather than
// value-based so that ADDING an offending field fails, not merely populating
// one. Mutation-checked: renaming ContainerID to Command turns it red.
func TestSandboxPayloadsArePayloadFree(t *testing.T) {
	banned := []string{
		"command", "cmd", "argv", "args", "arg",
		"env", "environ", "secret", "token",
		"output", "stdout", "stderr", "result", "body", "content",
		"err", "error", "message", "msg", "path", "image", "workdir",
		"tenant", "tenantid", "loop", "loopid", "node", "nodeid", "principal",
	}
	types := []reflect.Type{
		reflect.TypeOf(SandboxAcquirePayload{}),
		reflect.TypeOf(SandboxReleasePayload{}),
		reflect.TypeOf(SandboxHealthPayload{}),
		reflect.TypeOf(SandboxAdmissionPayload{}),
	}
	norm := func(s string) string {
		return strings.ToLower(strings.NewReplacer("_", "", "-", "", ",omitempty", "").Replace(s))
	}
	for _, ty := range types {
		for i := 0; i < ty.NumField(); i++ {
			f := ty.Field(i)
			names := []string{norm(f.Name), norm(strings.Split(f.Tag.Get("json"), ",")[0])}
			for _, n := range names {
				for _, b := range banned {
					if n == b {
						t.Errorf("%s.%s: field %q is banned from a payload-free sandbox payload", ty.Name(), f.Name, n)
					}
				}
			}
			// Only bounded scalars are admissible: no maps, slices, structs,
			// interfaces, or pointers, each of which can smuggle unbounded data.
			switch f.Type.Kind() {
			case reflect.String, reflect.Bool, reflect.Int, reflect.Int64:
			default:
				t.Errorf("%s.%s: kind %s is not a bounded scalar", ty.Name(), f.Name, f.Type.Kind())
			}
		}
	}
}
