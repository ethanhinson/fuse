// Package event defines the typed loop event stream — the canonical, subscribable
// record of everything the agent loop does (change 0043). It is an agent-free leaf
// package: the Event envelope, the Kind discriminant, the per-kind payload structs,
// and the EventStore interface live here and import neither internal/agent nor
// internal/model, so anything (including internal/tools) may import it without a
// cycle. Heavy content (assistant responses, tool args/results, structured spawn
// values) is carried as strings or json.RawMessage, never as agent/model types.
//
// The filesystem-backed implementation lives in the subpackage
// internal/event/fsstore, mirroring the internal/segment / internal/segment/fssink
// split (ADR-0017); the process-global holder + injection live in cmd/fuse
// (ADR-0019).
package event

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Seq is the monotonic per-session sequence number that orders events and serves
// as the Replay cursor. It is allocated by the store on Append; callers never set
// it.
type Seq uint64

// Kind is the discriminant of the event union. The string values are a durable
// wire format (events.jsonl is replayed) — see event_test.go, which pins them.
type Kind string

const (
	KindTurnStart      Kind = "turn.start"
	KindTurnEnd        Kind = "turn.end"
	KindModelCallStart Kind = "model.call.start"
	KindModelDelta     Kind = "model.delta" // streaming token delta (Stage B)
	KindModelCallEnd   Kind = "model.call.end"
	KindToolCall       Kind = "tool.call"
	KindToolResult     Kind = "tool.result"
	KindSpawnStart     Kind = "spawn.start"
	KindSpawnDone      Kind = "spawn.done"
	KindSummarize      Kind = "context.summarize"
	KindLoopTrip       Kind = "loop.detector.trip"
	// KindNoProgress fires when a run has gone too many tool calls without a file
	// change and the loop nudges the model. It never ends the run.
	KindNoProgress Kind = "no.progress"
	KindError      Kind = "error"
	// KindUserInput records a user/human turn entering the model-facing transcript:
	// the initial task at StartLoop and each human message injected at a turn
	// boundary (ADR-0022 self-pull). It exists because user input is otherwise NOT
	// in the durable stream — the injector appends a Role:"user" message to the
	// in-memory transcript but emits no event — so a transcript rebuilt purely from
	// events (change 0054's resume fold) would drop every user turn. Emitting it
	// keeps the event stream the single source of truth for reconstruction: Content
	// is the exact message text appended to the transcript (the injector's batched,
	// prefixed form for human turns; the raw task for the initial turn). Skipped for
	// a resumed loop's seed history, whose user turns are already in the stream.
	KindUserInput Kind = "user.input"
	// KindPermissionDecision records one permission-gate resolution: the verdict
	// (allow/ask/deny), the pipeline layer that decided it, and a bounded command
	// preview. It exists because gate behaviour was otherwise invisible in the
	// durable stream — denials only surfaced as tool-result strings and asks
	// (human prompts) not at all, so prompt frequency, the core auto-mode UX
	// metric, could not be measured (change 0067). An ask is emitted BEFORE the
	// approval func runs and the human outcome after it as a second layer=human
	// event, so a gate whose approver is AlwaysApprove shows as back-to-back ask→allow.
	KindPermissionDecision Kind = "permission.decision"
)

// Event is the stable envelope over every loop state transition. Payload is the
// kind-specific body marshaled to RawMessage so the envelope shape never changes
// and consumers decode only the kinds they care about.
type Event struct {
	Seq      Seq             `json:"seq"`
	TS       time.Time       `json:"ts"`
	NodeID   string          `json:"node_id,omitempty"`
	ParentID string          `json:"parent_id,omitempty"`
	Depth    int             `json:"depth,omitempty"`
	Turn     int             `json:"turn"`
	Kind     Kind            `json:"kind"`
	Payload  json.RawMessage `json:"payload,omitempty"`
	Trace    *TraceCarrier   `json:"trace,omitempty"`
}

// TraceCarrier is the repository-owned, durable subset of W3C trace context.
// It intentionally excludes baggage and provider-specific fields: telemetry
// propagation must not turn the event log into a header store.
type TraceCarrier struct {
	TraceParent string `json:"traceparent"`
	TraceState  string `json:"tracestate,omitempty"`
}

// MarshalJSON rejects invalid context before it can enter the durable event log.
func (c TraceCarrier) MarshalJSON() ([]byte, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	type wire TraceCarrier
	return json.Marshal(wire(c))
}

// UnmarshalJSON rejects malformed durable trace context on replay.
func (c *TraceCarrier) UnmarshalJSON(b []byte) error {
	type wire TraceCarrier
	var decoded wire
	if err := json.Unmarshal(b, &decoded); err != nil {
		return err
	}
	*c = TraceCarrier(decoded)
	return c.Validate()
}

// Validate verifies the W3C traceparent and optional tracestate syntax carried
// by Fuse. Only this small, portable context is persisted with an event.
func (c TraceCarrier) Validate() error {
	if !validTraceParent(c.TraceParent) {
		return fmt.Errorf("invalid traceparent")
	}
	if !validTraceState(c.TraceState) {
		return fmt.Errorf("invalid tracestate")
	}
	return nil
}

func validTraceParent(v string) bool {
	parts := strings.Split(v, "-")
	if len(parts) != 4 || len(parts[0]) != 2 || len(parts[1]) != 32 || len(parts[2]) != 16 || len(parts[3]) != 2 {
		return false
	}
	if parts[0] == "ff" || !lowerHex(parts[0]) || !lowerHex(parts[1]) || !lowerHex(parts[2]) || !lowerHex(parts[3]) {
		return false
	}
	return !allZero(parts[1]) && !allZero(parts[2])
}

func lowerHex(v string) bool {
	for _, r := range v {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

func allZero(v string) bool {
	for _, r := range v {
		if r != '0' {
			return false
		}
	}
	return true
}

func validTraceState(v string) bool {
	if v == "" {
		return true
	}
	if len(v) > 512 {
		return false
	}
	members := strings.Split(v, ",")
	if len(members) > 32 {
		return false
	}
	for _, member := range members {
		member = strings.Trim(member, " \t")
		if member == "" {
			return false
		}
		key, value, ok := strings.Cut(member, "=")
		if !ok || !validTraceStateKey(key) || !validTraceStateValue(value) {
			return false
		}
	}
	return true
}

func validTraceStateKey(key string) bool {
	if len(key) == 0 || len(key) > 256 {
		return false
	}
	for i, r := range key {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == '*' || r == '/' || (r == '@' && i > 0) {
			continue
		}
		return false
	}
	return key[0] >= 'a' && key[0] <= 'z'
}

func validTraceStateValue(value string) bool {
	if len(value) == 0 || len(value) > 256 || value[0] == ' ' || value[len(value)-1] == ' ' {
		return false
	}
	for _, r := range value {
		if r < 0x20 || r > 0x7e || r == ',' || r == '=' {
			return false
		}
	}
	return true
}

// MarshalJSONL encodes the event as a single JSON object with NO trailing newline
// (the store adds the record delimiter). Each line is self-delimiting: any newline
// inside a payload body is JSON-escaped, so a record boundary can never be forged
// from content (learning: self-delimiting-serialization-for-round-trip).
func (e Event) MarshalJSONL() ([]byte, error) {
	return json.Marshal(e)
}

// ParseEvent decodes one JSONL line back into an Event.
func ParseEvent(line []byte) (Event, error) {
	var e Event
	err := json.Unmarshal(line, &e)
	return e, err
}

// MarshalPayload marshals a per-kind payload struct to a RawMessage for embedding
// in Event.Payload. The model judges which struct to pass; this helper is dumb.
func MarshalPayload(v any) (json.RawMessage, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(b), nil
}

// ---- per-kind payload structs (agent/model-free) ----

// ToolCallRef is a lightweight reference to a tool call inside a model response.
type ToolCallRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type TurnStartPayload struct {
	Turn int `json:"turn"`
}

type TurnEndPayload struct {
	Turn int `json:"turn"`
}

type ModelCallStartPayload struct {
	Model    string `json:"model"`
	MsgCount int    `json:"msg_count"`
}

type ModelCallEndPayload struct {
	Content      string `json:"content"`
	InputTokens  int    `json:"input_tokens"`
	CachedTokens int    `json:"cached_tokens,omitempty"` // of input_tokens, served from the provider's prefix cache
	OutputTokens int    `json:"output_tokens"`
	// ReasoningTokens is how many of output_tokens went to reasoning, when reported.
	ReasoningTokens int           `json:"reasoning_tokens,omitempty"`
	ToolCalls       []ToolCallRef `json:"tool_calls,omitempty"`
	// FinishReason is the gateway's finish_reason for the reply ("stop",
	// "length", ...); "length" marks a reply cut at the output cap.
	FinishReason string `json:"finish_reason,omitempty"`
	// Discarded marks a reply the loop threw away because it was cut off and
	// unusable. Its tokens still count, but it never enters the transcript, so a
	// resume must not fold it back in.
	Discarded bool `json:"discarded,omitempty"`
}

// ModelDeltaPayload carries one incremental token chunk (Stage B).
type ModelDeltaPayload struct {
	Text string `json:"text"`
}

type ToolCallPayload struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

type ToolResultPayload struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Result  string `json:"result"`
	IsError bool   `json:"is_error,omitempty"`
	// Synthetic marks a result injected by the loop machinery rather than
	// produced by a real tool execution (e.g. a manufactured failure for an
	// orphaned spawn).
	Synthetic bool `json:"synthetic,omitempty"`
}

type SpawnStartPayload struct {
	ChildNodeID string `json:"child_node_id"`
	Label       string `json:"label,omitempty"`
	Task        string `json:"task,omitempty"`
	Role        string `json:"role,omitempty"`
	// Pool names the child pool/queue the spawn was dispatched to, when the
	// loop runs workers in a structured pool rather than ad hoc goroutines.
	Pool string `json:"pool,omitempty"`
}

// SpawnDonePayload carries everything the session-log projection needs to
// reproduce the current child-completion LogEntry, plus the spawn result.
// Structured is the child's structured-delegation value (change 0042) as raw JSON.
type SpawnDonePayload struct {
	ChildNodeID string          `json:"child_node_id"`
	ParentID    string          `json:"parent_id,omitempty"`
	Label       string          `json:"label,omitempty"`
	Depth       int             `json:"depth,omitempty"`
	Result      string          `json:"result,omitempty"`
	Err         string          `json:"err,omitempty"`
	Structured  json.RawMessage `json:"structured,omitempty"`
	// Orphaned marks a completion that arrived after the parent loop stopped
	// tracking the child — the rebuild must not wait on or count it.
	Orphaned bool `json:"orphaned,omitempty"`
}

type SummarizePayload struct {
	TurnStart    int      `json:"turn_start"`
	TurnEnd      int      `json:"turn_end"`
	ToolNames    []string `json:"tool_names,omitempty"`
	TokensBefore int      `json:"tokens_before"`
	TokensAfter  int      `json:"tokens_after"`
	Pointer      string   `json:"pointer,omitempty"`
	// Summary carries the actual summarized text (rebuilt from the stream by the
	// resume fold). InsertAt is the transcript position the rebuilt summary was
	// inserted at, so a rebuild reproduces the exact compaction layout.
	Summary  string `json:"summary,omitempty"`
	InsertAt int    `json:"insert_at,omitempty"`
}

type LoopTripPayload struct {
	Turn int `json:"turn"`
	// Reason, when non-empty, distinguishes a policy-denial repeat trip (change
	// 0067: the model kept re-issuing a policy-blocked call after the injected
	// nudge) from the generic identical-call doom loop. Additive: absent on
	// generic trips, so pre-0067 streams replay unchanged.
	Reason string `json:"reason,omitempty"`
}

type ErrorPayload struct {
	Err  string `json:"err"`
	Turn int    `json:"turn"`
}

// PermissionDecisionPayload accompanies KindPermissionDecision. Command is a
// bounded preview (the bash command truncated to ~200 chars, or a truncated
// args preview for other tools) — never full args, which tool.call already
// carries. Layer names the deciding pipeline stage (permissions.Layer*
// constants); Verdict is "allow" | "ask" | "deny".
type PermissionDecisionPayload struct {
	ToolCallID string `json:"tool_call_id,omitempty"`
	Tool       string `json:"tool"`
	Verdict    string `json:"verdict"`
	Layer      string `json:"layer"`
	Reason     string `json:"reason,omitempty"`
	Mode       string `json:"mode"`
	Command    string `json:"command,omitempty"`
	// DecidedBy disambiguates human-layer outcomes, taken from the gate's
	// approver: "human" (a person answered the prompt) or "policy" (a stand-in
	// such as AlwaysApprove answered with no human present). Empty on other
	// layers except a rules-layer deny forced by a policy approver.
	DecidedBy string `json:"decided_by,omitempty"`
	// Classifier is present when the auto-mode classifier was consulted for
	// this resolution: the audit record of which model judged and whether its
	// reply was usable. Bounded fields only; never prompt or reply text.
	Classifier *ClassifierCallPayload `json:"classifier,omitempty"`
	// Segments carries the per-segment deterministic verdict for a COMPOUND
	// bash command (change 0142): one element per parsed segment, so a compound
	// decision stays attributable to the specific part that drove an ask or
	// deny — not just the raw command string. Omitted for non-compound commands
	// and non-bash tools. Bounded: the parser caps segment count by construction.
	Segments []SegmentVerdictPayload `json:"segments,omitempty"`
}

// SegmentVerdictPayload is one segment of a compound bash command's
// deterministic decomposition (change 0142): the segment's command name, the
// verdict the deterministic layers reached for it, and the layer-naming reason
// when it did not allow. Bounded identity only — never the segment's args or
// raw text beyond the name.
type SegmentVerdictPayload struct {
	Name    string `json:"name"`
	Verdict string `json:"verdict"`
	Reason  string `json:"reason,omitempty"`
}

// ClassifierCallPayload is the classifier-consultation slice of a
// PermissionDecisionPayload (audit completeness, change 0069 follow-up).
// Truncated marks a reply that hit the classifier token cap; ParseOK is false
// when no verdict could be parsed and the outcome fell closed to ask; Cached
// means the verdict came from the per-session cache (token/latency fields then
// describe the original call).
type ClassifierCallPayload struct {
	Model        string `json:"model"`
	LatencyMS    int64  `json:"latency_ms"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
	Truncated    bool   `json:"truncated,omitempty"`
	ParseOK      bool   `json:"parse_ok"`
	Cached       bool   `json:"cached,omitempty"`
}

// The sandbox payloads (change 0063) are PAYLOAD-FREE by construction: they
// carry only bounded identity (handler, runtime, short container id) and
// closed-enum classification. They never carry the command, its arguments, any
// environment key or value, process output, or a raw error string — all of
// which are unbounded and model- or attacker-influenced, and events.jsonl is
// durable and replayed. Correlation comes from the ENVELOPE — Event.NodeID plus
// the StreamKey{Tenant, Loop} the event is appended under — and is never
// duplicated into a payload, which would create a second source of truth.
// event_test.go enforces both properties structurally.

// SandboxCause is the closed enum of why an execution context stopped being
// held. It is the wire form of sandbox.ReleaseCause; that package imports this
// one, so the values cannot be referenced from here and are instead pinned
// literally and kept in lockstep (event_test.go pins all five).
type SandboxCause string

const (
	// SandboxCauseReleased is the ordinary hand-back; the context stays warm.
	SandboxCauseReleased SandboxCause = "released"
	// SandboxCauseLoopEnd is teardown driven by the loop or the pool ending.
	SandboxCauseLoopEnd SandboxCause = "loop_end"
	// SandboxCauseEarlyReturn is a release from an error path that returned
	// before the work completed — kept distinct so a leak on an early return is
	// visible rather than indistinguishable from normal traffic.
	SandboxCauseEarlyReturn SandboxCause = "early_return"
	// SandboxCauseIdleTTL is the reaper reclaiming a context idle past its TTL.
	SandboxCauseIdleTTL SandboxCause = "idle_ttl"
	// SandboxCauseStaleCheckout is a warm context the pool could not certify for
	// reuse — the re-asserted principal key did not match, or the scrubbed
	// environment could not be re-applied — and therefore discarded rather than
	// handed out. Its firing at all means an isolation invariant was violated:
	// alert on it, do not tune it.
	SandboxCauseStaleCheckout SandboxCause = "stale_checkout"
	// SandboxCauseOrphan is a remote sandbox collected by the substrate-wide
	// reaper because no live instance was heartbeating it (change 0075). Distinct
	// from idle_ttl: this one means an instance died without releasing, which is
	// the only signal that fact produces.
	SandboxCauseOrphan SandboxCause = "orphan"
)

// SandboxAdmissionOutcome is the closed enum of SandboxAdmissionPayload.Outcome.
// The values are the wire form of sandbox.AdmissionInfo's disposition; the
// sandbox package imports this one, so they are kept in lockstep by the
// translator in internal/tools and pinned in event_test.go.
type SandboxAdmissionOutcome = string

const (
	// SandboxAdmissionQueued is an Exec that waited past the note threshold and
	// then ran — the "backpressure is happening" signal.
	SandboxAdmissionQueued SandboxAdmissionOutcome = "queued"
	// SandboxAdmissionRefused is an Exec refused because the host was at capacity
	// — the runaway signal, and the alert target.
	SandboxAdmissionRefused SandboxAdmissionOutcome = "refused"
)

// SandboxHealthReason is the closed enum of SandboxHealthPayload.Reason.
//
// These are named constants rather than the doc comment they used to be
// (change 0065, task 7) for the same reason SandboxCause and
// SandboxAdmissionOutcome are. Declaring them
// here also gives the translator in internal/tools an exhaustive switch to be
// checked against, and event_test.go a set to pin.
//
// Only the reasons a substrate can honestly observe are declared: the original
// four, plus floor_unverified for the remote substrate whose canary pair did not
// prove the floor (change 0075).
// "unresponsive" and "recovered" presuppose a container that outlives an Exec,
// which `docker run --rm` per Exec does not provide; they are deferred to change
// #74 along with a real ContainerID, and declaring constants for them here would
// invite an emitter to synthesise a transition nothing observed.
type SandboxHealthReason = string

const (
	// SandboxHealthOOM is a container killed by the kernel's OOM killer.
	SandboxHealthOOM SandboxHealthReason = "oom"
	// SandboxHealthRuntimeExit is a container that died to some other signal, or
	// a runtime that could not start the command at all.
	SandboxHealthRuntimeExit SandboxHealthReason = "runtime_exit"
	// SandboxHealthPullFailed is image acquisition failing: no container can
	// start on this substrate until a later pull succeeds.
	SandboxHealthPullFailed SandboxHealthReason = "pull_failed"
	// SandboxHealthAcquireFailed is a cold start that produced no Runner.
	SandboxHealthAcquireFailed SandboxHealthReason = "acquire_failed"
	// SandboxHealthFloorUnverified is a remote substrate whose network floor the
	// canary pair could not prove (change 0075). Alert on it: it means the
	// cluster's CNI does not enforce NetworkPolicy, so the metadata-deny floor
	// does not exist and no sandbox on that cluster is contained.
	SandboxHealthFloorUnverified SandboxHealthReason = "floor_unverified"
)

// UserInputPayload accompanies KindUserInput. Content is the exact user-turn text
// appended to the model-facing transcript for this turn — the raw initial task, or
// the human injector's batched/prefixed form for a mid-conversation Send. The resume
// fold (change 0054) maps this one-to-one to a {Role:"user", Content} message, so a
// transcript rebuilt from the durable stream matches the live in-memory transcript
// byte-for-byte.
type UserInputPayload struct {
	Turn    int    `json:"turn"`
	Content string `json:"content"`
	// Source names a non-human speaker for a steered turn; empty for a plain human or
	// task turn. Informational only.
	Source string `json:"source,omitempty"`
}

// NoProgressPayload accompanies KindNoProgress: Calls tool calls across the agent
// tree since the last file change, against a nudge interval of Threshold.
type NoProgressPayload struct {
	Turn      int   `json:"turn"`
	Calls     int64 `json:"calls"`
	Threshold int   `json:"threshold"`
}
