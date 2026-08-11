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
	KindError          Kind = "error"
	// KindLoopParked marks an interactive (persistent conversational) loop reaching a
	// terminal turn and PARKING to await the next human input, rather than finishing.
	// It is the deterministic "this exchange is complete, send your next message"
	// signal a conversational client needs — the alternative (guessing completion from
	// the absence of further events) is unreliable. Emitted only in interactive mode
	// just before the park; never in a one-shot run.
	KindLoopParked Kind = "loop.parked"
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
	Content      string        `json:"content"`
	InputTokens  int           `json:"input_tokens"`
	OutputTokens int           `json:"output_tokens"`
	ToolCalls    []ToolCallRef `json:"tool_calls,omitempty"`
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
}

type SpawnStartPayload struct {
	ChildNodeID string `json:"child_node_id"`
	Label       string `json:"label,omitempty"`
	Task        string `json:"task,omitempty"`
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
}

type SummarizePayload struct {
	TurnStart    int      `json:"turn_start"`
	TurnEnd      int      `json:"turn_end"`
	ToolNames    []string `json:"tool_names,omitempty"`
	TokensBefore int      `json:"tokens_before"`
	TokensAfter  int      `json:"tokens_after"`
	Pointer      string   `json:"pointer,omitempty"`
}

type LoopTripPayload struct {
	Turn int `json:"turn"`
}

type ErrorPayload struct {
	Err  string `json:"err"`
	Turn int    `json:"turn"`
}

// LoopParkedPayload accompanies KindLoopParked. Content carries the assistant's final
// answer text for the exchange just completed (the terminal no-tool response), so a
// conversational client can render the reply directly from this one event instead of
// reconstructing which model.call.end was terminal. Turn is the turn that produced it.
type LoopParkedPayload struct {
	Turn    int    `json:"turn"`
	Content string `json:"content"`
}
