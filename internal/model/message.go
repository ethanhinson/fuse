// Package model provides the gateway adapter and the message/tool types
// shared across the harness.
package model

import "encoding/json"

// Message is a single chat message in OpenAI-compatible form.
type Message struct {
	Role       string
	Content    string
	ToolCalls  []ToolCall
	ToolCallID string // set when Role == "tool"
	Name       string // tool name, for tool-result messages
}

// ToolCall is a function call requested by the model. Arguments is the raw
// JSON argument string exactly as the model produced it.
type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// ToolSchema is a tool advertised to the model. Parameters is a JSON Schema
// object.
type ToolSchema struct {
	Name        string
	Description string
	Parameters  map[string]any
}

// CompletionReq is a single completion request.
type CompletionReq struct {
	Model      string
	Messages   []Message
	Tools      []ToolSchema
	MaxTokens  int
	ToolChoice string // "auto" (default), "none" (force text), "required"
}

// CompletionResp is the assistant's reply.
type CompletionResp struct {
	Content      string
	ToolCalls    []ToolCall
	InputTokens  int // prompt tokens reported by the gateway
	CachedTokens int // of InputTokens, how many the provider served from its prefix cache (0 when unreported)
	OutputTokens int // completion tokens reported by the gateway
	// FinishReason is the gateway's choice-level finish_reason ("stop",
	// "length", "tool_calls", ...). "length" means the reply was cut at
	// max_tokens: whatever it says is incomplete, and a missing tool call is
	// not a decision to stop. Empty when the gateway did not report one.
	FinishReason string
}

// AsMessage converts the response into an assistant Message for appending to
// the running conversation. A tool call whose arguments are not valid JSON is
// still executed as the model sent it (the registry answers "bad arguments"
// so the model can retry), but the history copy wraps the raw text in a valid
// JSON object: some providers validate every tool call in the transcript and
// reject the whole request otherwise.
func (r CompletionResp) AsMessage() Message {
	if len(r.ToolCalls) == 0 {
		return Message{Role: "assistant", Content: r.Content}
	}
	calls := make([]ToolCall, len(r.ToolCalls))
	for i, tc := range r.ToolCalls {
		if !json.Valid([]byte(tc.Arguments)) {
			wrapped, _ := json.Marshal(map[string]string{"malformed_arguments": tc.Arguments})
			tc.Arguments = string(wrapped)
		}
		calls[i] = tc
	}
	return Message{Role: "assistant", Content: r.Content, ToolCalls: calls}
}
