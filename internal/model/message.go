// Package model provides the gateway adapter and the message/tool types
// shared across the harness.
package model

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
	OutputTokens int // completion tokens reported by the gateway
}

// AsMessage converts the response into an assistant Message for appending to
// the running conversation.
func (r CompletionResp) AsMessage() Message {
	return Message{Role: "assistant", Content: r.Content, ToolCalls: r.ToolCalls}
}
