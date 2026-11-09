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
	// Reasoning is an assistant reply's reasoning text, kept only when the model
	// is configured to have it sent back (CompletionReq.ReplayReasoning).
	// ReasoningField is the wire field it arrived in (reasoning_content,
	// reasoning or reasoning_text), so it goes back in the field the provider
	// uses; empty means reasoning_content.
	Reasoning      string
	ReasoningField string
	// Attachments are images sent with this message's text (a user message, or
	// a tool result such as read_file on a PNG).
	Attachments []Attachment
}

// AttachmentTokenEstimate is the planning cost of one image in tokens. Providers
// differ widely: Gemini charged about 1,100 prompt tokens for a 32x32 image,
// Haiku and Kimi a few. 1,600 is Anthropic's cost at its 1568 px auto-resize
// ceiling, a safe estimate for context accounting and the rate gate.
const AttachmentTokenEstimate = 1600

// Attachment is an image sent to the model with a message's text.
type Attachment struct {
	MediaType     string // image/png | image/jpeg | image/gif | image/webp
	Data          []byte `json:"-"` // raw bytes; empty means not sendable (see adapter)
	Name          string // path as given by the model or user
	Ref           string // art://.../attachment/<id>, "" when not stored
	Size          int
	SHA256        string // hex of Data
	Width, Height int    // 0 when unknown (WebP)
}

// Label is how text refers to the attachment: Ref when stored, else Name.
func (a Attachment) Label() string {
	if a.Ref != "" {
		return a.Ref
	}
	return a.Name
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
	// ReasoningEffort is sent as reasoning_effort when set.
	ReasoningEffort string
	// ReplayReasoning sends each assistant message's Reasoning back in its
	// native field. Models whose chat template expects their earlier reasoning
	// inside a tool-call loop (DeepSeek, Kimi and others) otherwise start every
	// call without it. Off sends no reasoning. The CLI sets it unless the model's
	// config turns it off.
	ReplayReasoning bool
	// Dialect names the model family whose wire behaviour applies (see
	// dialect.go). Empty detects it from Model.
	Dialect string
	// DialectOptions tune the dialect's capabilities (config dialect_options:).
	// Zero keeps the family defaults.
	DialectOptions DialectOptions
	// Attachments reports whether the model accepts images. When false the
	// adapter sends no image parts and each attachment goes as a text note, so a
	// text-only model never receives image bytes.
	Attachments bool
}

// CompletionResp is the assistant's reply.
type CompletionResp struct {
	Content      string
	ToolCalls    []ToolCall
	InputTokens  int // prompt tokens reported by the gateway
	CachedTokens int // of InputTokens, how many the provider served from its prefix cache (0 when unreported)
	CacheWrite   int // of InputTokens, how many were written to the provider's cache (0 when unreported)
	OutputTokens int // completion tokens reported by the gateway
	// ReasoningTokens is how many of OutputTokens went to reasoning, 0 when unreported.
	ReasoningTokens int
	// Reasoning is the reply's reasoning text where the gateway returned it, and
	// ReasoningField the wire field it came in.
	Reasoning      string
	ReasoningField string
	// FinishReason is the gateway's choice-level finish_reason ("stop",
	// "length", "tool_calls", ...). "length" means the reply was cut at
	// max_tokens: whatever it says is incomplete, and a missing tool call is
	// not a decision to stop. Empty when the gateway did not report one.
	FinishReason string
}

// Truncated reports whether the reply was cut at its output cap. limit is the
// max_tokens the request sent. A gateway that reports no finish_reason is only
// judged truncated when the cap was spent and nothing came back: output tokens
// alone cannot decide it, because a subprocess-backed model does not honor the cap
// and can legitimately report more.
func (r CompletionResp) Truncated(limit int) bool {
	switch r.FinishReason {
	case "length", "max_tokens":
		return true
	case "":
		return limit > 0 && r.OutputTokens >= limit && r.Content == "" && len(r.ToolCalls) == 0
	}
	return false
}

// AsMessage converts the response into an assistant Message for appending to
// the running conversation. A tool call whose arguments are not valid JSON is
// still executed as the model sent it (the registry answers "bad arguments"
// so the model can retry), but the history copy wraps the raw text in a valid
// JSON object: some providers validate every tool call in the transcript and
// reject the whole request otherwise.
func (r CompletionResp) AsMessage() Message {
	if len(r.ToolCalls) == 0 {
		return Message{Role: "assistant", Content: r.Content, Reasoning: r.Reasoning, ReasoningField: r.ReasoningField}
	}
	calls := make([]ToolCall, len(r.ToolCalls))
	for i, tc := range r.ToolCalls {
		if !json.Valid([]byte(tc.Arguments)) {
			wrapped, _ := json.Marshal(map[string]string{"malformed_arguments": tc.Arguments})
			tc.Arguments = string(wrapped)
		}
		calls[i] = tc
	}
	return Message{Role: "assistant", Content: r.Content, ToolCalls: calls, Reasoning: r.Reasoning, ReasoningField: r.ReasoningField}
}
