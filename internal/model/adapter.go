package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Adapter is an OpenAI-compatible client for the LiteLLM gateway.
type Adapter struct {
	baseURL string
	key     string
	hc      *http.Client
	trace   io.Writer // when non-nil, raw JSON req/resp are written here
}

// NewAdapter builds an Adapter. baseURL is the gateway root, e.g.
// "http://localhost:4000/v1". If hc is nil, http.DefaultClient is used.
func NewAdapter(baseURL, key string, hc *http.Client) *Adapter {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &Adapter{baseURL: strings.TrimRight(baseURL, "/"), key: key, hc: hc}
}

// WithTrace returns a copy of the adapter that writes raw JSON request and
// response bodies to w (e.g. os.Stderr). Each request/response pair is
// surrounded by ── REQ ── / ── RESP ── markers.
func (a *Adapter) WithTrace(w io.Writer) *Adapter {
	cp := *a
	cp.trace = w
	return &cp
}

// wire types mirror the OpenAI-compatible JSON payloads.
type wireMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content,omitempty"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	Name       string         `json:"name,omitempty"`
}

type wireToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type wireTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	} `json:"function"`
}

type wireReq struct {
	Model      string        `json:"model"`
	Messages   []wireMessage `json:"messages"`
	Tools      []wireTool    `json:"tools,omitempty"`
	MaxTokens  int           `json:"max_tokens,omitempty"`
	ToolChoice string        `json:"tool_choice,omitempty"`
}

type wireResp struct {
	Choices []struct {
		Message wireMessage `json:"message"`
	} `json:"choices"`
}

func prettyJSON(b []byte) []byte {
	var buf bytes.Buffer
	if err := json.Indent(&buf, b, "", "  "); err != nil {
		return b
	}
	return buf.Bytes()
}

// Complete sends a completion request and returns the assistant reply.
func (a *Adapter) Complete(ctx context.Context, req CompletionReq) (CompletionResp, error) {
	payload := wireReq{Model: req.Model, MaxTokens: req.MaxTokens, ToolChoice: req.ToolChoice}
	for _, m := range req.Messages {
		wm := wireMessage{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID, Name: m.Name}
		for _, tc := range m.ToolCalls {
			w := wireToolCall{ID: tc.ID, Type: "function"}
			w.Function.Name = tc.Name
			w.Function.Arguments = tc.Arguments
			wm.ToolCalls = append(wm.ToolCalls, w)
		}
		payload.Messages = append(payload.Messages, wm)
	}
	for _, t := range req.Tools {
		wt := wireTool{Type: "function"}
		wt.Function.Name = t.Name
		wt.Function.Description = t.Description
		wt.Function.Parameters = t.Parameters
		payload.Tools = append(payload.Tools, wt)
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return CompletionResp{}, err
	}
	if a.trace != nil {
		fmt.Fprintf(a.trace, "\n── REQ ──\n%s\n", prettyJSON(body))
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return CompletionResp{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+a.key)

	res, err := a.hc.Do(httpReq)
	if err != nil {
		return CompletionResp{}, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return CompletionResp{}, err
	}
	if a.trace != nil {
		fmt.Fprintf(a.trace, "\n── RESP ──\n%s\n", prettyJSON(raw))
	}
	if res.StatusCode != http.StatusOK {
		return CompletionResp{}, fmt.Errorf("gateway status %d: %s", res.StatusCode, strings.TrimSpace(string(raw)))
	}

	var wr wireResp
	if err := json.Unmarshal(raw, &wr); err != nil {
		return CompletionResp{}, fmt.Errorf("decode gateway response: %w", err)
	}
	if len(wr.Choices) == 0 {
		return CompletionResp{}, fmt.Errorf("gateway returned no choices")
	}
	msg := wr.Choices[0].Message
	out := CompletionResp{Content: msg.Content}
	for _, tc := range msg.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, ToolCall{ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments})
	}
	return out, nil
}
