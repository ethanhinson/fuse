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
		KindLoopHarness:        "loop.harness",
		KindMCPServer:          "mcp.server",
		KindLoopPrincipal:      "loop.principal",
		KindLoopWorkspace:      "loop.workspace",
		KindToolIdentityMinted: "tool.identity.minted",
		KindLoopCancelRequest:  "loop.cancel.request",
		KindLoopCancelled:      "loop.cancelled",
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

// TestLoopWorkspacePayloadRoundTrip round-trips a LoopWorkspacePayload through the
// store envelope and asserts the identity and provider survive (change 0129, the
// loop.workspace event a resume re-opens the same workspace from).
func TestLoopWorkspacePayloadRoundTrip(t *testing.T) {
	want := LoopWorkspacePayload{ID: "tenant-a/loop-1", Provider: "local"}
	raw, err := MarshalPayload(want)
	if err != nil {
		t.Fatalf("MarshalPayload: %v", err)
	}
	e := Event{Seq: 1, Kind: KindLoopWorkspace, Payload: raw}
	line, err := e.MarshalJSONL()
	if err != nil {
		t.Fatalf("MarshalJSONL: %v", err)
	}
	got, err := ParseEvent(line)
	if err != nil {
		t.Fatalf("ParseEvent: %v", err)
	}
	if got.Kind != KindLoopWorkspace {
		t.Fatalf("kind = %q, want loop.workspace", got.Kind)
	}
	var p LoopWorkspacePayload
	if err := json.Unmarshal(got.Payload, &p); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	if p.ID != want.ID || p.Provider != want.Provider {
		t.Errorf("payload round-trip:\n got %+v\nwant %+v", p, want)
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
		LoopWorkspacePayload{ID: "tenant-a/loop-1", Provider: "local"},
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
		// orphan (change 0075) is the sixth: a REMOTE sandbox the substrate-wide
		// reaper collected because no live instance was heartbeating it. It is
		// deliberately not folded into idle_ttl — idle_ttl is this process's Pool
		// reclaiming what it remembers, and an orphan is a sandbox no Pool
		// remembers, so the orphan count is the only signal that fuse instances
		// are dying without releasing.
		SandboxCauseOrphan: "orphan",
	}
	for c, want := range cases {
		if string(c) != want {
			t.Errorf("SandboxCause %q != %q", string(c), want)
		}
	}
	if len(cases) != 6 {
		t.Fatalf("cause enum must have exactly 6 values, got %d", len(cases))
	}
}

// TestSandboxHealthReasonWireValues pins the health-reason enum. These strings
// become a Prometheus label (fuse_sandbox_unhealthy_total{reason=...}) and are the
// wire form of sandbox.HealthReason; the sandbox package imports this one, so they
// are pinned literally here and kept in lockstep by the translator in
// internal/tools.
//
// FIVE values, not four: floor_unverified (change 0075) is the remote substrate's
// canary pair failing to prove the network floor, and it is the one on this list
// an operator should page on — it means that cluster's CNI does not enforce
// NetworkPolicy, so no sandbox on it is contained.
func TestSandboxHealthReasonWireValues(t *testing.T) {
	cases := map[SandboxHealthReason]string{
		SandboxHealthOOM:             "oom",
		SandboxHealthRuntimeExit:     "runtime_exit",
		SandboxHealthPullFailed:      "pull_failed",
		SandboxHealthAcquireFailed:   "acquire_failed",
		SandboxHealthFloorUnverified: "floor_unverified",
	}
	for r, want := range cases {
		if r != want {
			t.Errorf("SandboxHealthReason %q != %q", r, want)
		}
	}
	if len(cases) != 5 {
		t.Fatalf("health-reason enum must have exactly 5 values, got %d", len(cases))
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

func payloadKeys(t *testing.T, v any) map[string]any {
	t.Helper()
	raw, err := MarshalPayload(v)
	if err != nil {
		t.Fatalf("MarshalPayload: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return m
}

func assertExactKeys(t *testing.T, m map[string]any, want ...string) {
	t.Helper()
	if len(m) != len(want) {
		t.Fatalf("keys = %v, want exactly %v", m, want)
	}
	for _, k := range want {
		if _, ok := m[k]; !ok {
			t.Fatalf("missing key %q in %v", k, m)
		}
	}
}

func TestLoopPrincipalPayloadShape(t *testing.T) {
	m := payloadKeys(t, LoopPrincipalPayload{Subject: "s", ClientID: "c", Issuer: "i", Scopes: []string{}, TokenID: "t"})
	assertExactKeys(t, m, "subject", "client_id", "issuer", "scopes", "token_id")
	if _, ok := m["scopes"].([]any); !ok {
		t.Fatalf("scopes = %#v, want JSON array", m["scopes"])
	}
}

func TestToolIdentityMintedPayloadShape(t *testing.T) {
	m := payloadKeys(t, ToolIdentityMintedPayload{Subject: "s", Audience: "a", Scope: "x y", LoopID: "l"})
	assertExactKeys(t, m, "subject", "audience", "scope", "loop_id")
}

// TestSpawnStartPayloadJSONWithoutRole pins the wire bytes of a spawn.start
// payload that names no role: adding the role field must not change them.
func TestSpawnStartPayloadJSONWithoutRole(t *testing.T) {
	b, err := json.Marshal(SpawnStartPayload{ChildNodeID: "n2", Label: "researcher", Task: "do it"})
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"child_node_id":"n2","label":"researcher","task":"do it"}`
	if string(b) != want {
		t.Fatalf("spawn.start JSON = %s, want %s", b, want)
	}
}

// TestSpawnStartPayloadRoleRoundTrip: a role spawn's spawn.start carries the
// role name on the wire and survives a decode.
func TestSpawnStartPayloadRoleRoundTrip(t *testing.T) {
	in := SpawnStartPayload{ChildNodeID: "n2", Role: "reviewer"}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"role":"reviewer"`) {
		t.Fatalf("spawn.start JSON = %s, want it to contain \"role\":\"reviewer\"", b)
	}
	var out SpawnStartPayload
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Fatalf("round trip = %+v, want %+v", out, in)
	}
}

func TestLoopCancelledPayloadJSON(t *testing.T) {
	b, err := json.Marshal(LoopCancelledPayload{Reason: "operator stop", RequestedBy: "alice", WasLive: false})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b), `{"reason":"operator stop","requested_by":"alice","was_live":false}`; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
	b, err = json.Marshal(LoopCancelledPayload{Reason: "operator stop", RequestedBy: "alice", RequestSeq: 7})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"request_seq":7`) {
		t.Errorf("missing request_seq: %s", b)
	}
}

func TestLoopCancelRequestPayloadJSON(t *testing.T) {
	b, err := json.Marshal(LoopCancelRequestPayload{Reason: "r", RequestedBy: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b), `{"reason":"r","requested_by":"bob"}`; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

// TestPayloadAdditionsRoundTrip (change 0127, task 1) covers the additive payload
// fields added for stream-rebuilt loop state: SummarizePayload.Summary/InsertAt,
// SpawnDonePayload.Orphaned, ToolResultPayload.Synthetic, and
// SpawnStartPayload.Pool. Each is a new-field-set value round-tripped through
// MarshalPayload/ParseEvent, and each is decoded back into zero-value form via
// json.Unmarshal so the assertion is field-by-field, not raw-bytes equality.
func TestPayloadAdditionsRoundTrip(t *testing.T) {
	cases := []any{
		SummarizePayload{TurnStart: 0, TurnEnd: 3, TokensBefore: 100, TokensAfter: 20, Summary: "[compacted summary of turns 0..3]", InsertAt: 5},
		SpawnDonePayload{ChildNodeID: "n2", ParentID: "n1", Result: "done", Orphaned: true},
		ToolResultPayload{ID: "c1", Name: "read_file", Result: "data", Synthetic: true},
		SpawnStartPayload{ChildNodeID: "n2", Label: "researcher", Task: "do it", Pool: "workers"},
	}
	for i, in := range cases {
		raw, err := MarshalPayload(in)
		if err != nil {
			t.Fatalf("case %d marshal: %v", i, err)
		}
		out := reflect.New(reflect.TypeOf(in)).Interface()
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("case %d unmarshal: %v", i, err)
		}
		if !reflect.DeepEqual(reflect.ValueOf(out).Elem().Interface(), in) {
			t.Errorf("case %d round trip = %+v, want %+v", i, reflect.ValueOf(out).Elem().Interface(), in)
		}
	}
}

// TestPayloadAdditionsAbsentAreZero (change 0127, task 1) simulates an OLD
// stream: a payload that never emitted the new fields. Decoding it must not
// error and the new fields must be zero-valued, so old events.jsonl replays
// unchanged against the widened payload structs.
func TestPayloadAdditionsAbsentAreZero(t *testing.T) {
	old := []json.RawMessage{
		// Pre-0127 SummarizePayload: no summary, no insert_at.
		json.RawMessage(`{"turn_start":0,"turn_end":3,"tool_names":["bash"],"tokens_before":100,"tokens_after":20,"pointer":"/p"}`),
		// Pre-0127 SpawnDonePayload: no orphaned.
		json.RawMessage(`{"child_node_id":"n2","parent_id":"n1","label":"researcher","depth":1,"result":"done"}`),
		// Pre-0127 ToolResultPayload: no synthetic.
		json.RawMessage(`{"id":"c1","name":"read_file","result":"data","is_error":false}`),
		// Pre-0127 SpawnStartPayload: no pool.
		json.RawMessage(`{"child_node_id":"n2","label":"researcher","task":"do it"}`),
	}
	targets := []reflect.Value{
		reflect.ValueOf(&SummarizePayload{}),
		reflect.ValueOf(&SpawnDonePayload{}),
		reflect.ValueOf(&ToolResultPayload{}),
		reflect.ValueOf(&SpawnStartPayload{}),
	}
	newFields := map[string][]string{
		"SummarizePayload": {"Summary", "InsertAt"},
		"SpawnDonePayload": {"Orphaned"},
		"ToolResultPayload": {"Synthetic"},
		"SpawnStartPayload": {"Pool"},
	}
	for i, raw := range old {
		tgt := targets[i]
		if err := json.Unmarshal(raw, tgt.Interface()); err != nil {
			t.Fatalf("case %d unmarshal old stream: %v", i, err)
		}
		orig := tgt.Elem()
		for _, name := range newFields[orig.Type().Name()] {
			field := orig.FieldByName(name)
			zero := reflect.Zero(field.Type())
			if got := field.Interface(); !reflect.DeepEqual(got, zero.Interface()) {
				t.Errorf("case %d %s.%s = %#v, want zero", i, orig.Type().Name(), name, got)
			}
		}
	}
}
