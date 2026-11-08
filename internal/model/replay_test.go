package model

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fixedServer records each request body and answers every request with body,
// as an SSE stream when stream is true and as one JSON document otherwise.
func fixedServer(t *testing.T, bodies *[]map[string]any, stream bool, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		*bodies = append(*bodies, req)
		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
		} else {
			w.Header().Set("Content-Type", "application/json")
		}
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func sse(chunks ...string) string {
	var b strings.Builder
	for _, c := range chunks {
		b.WriteString("data: " + c + "\n\n")
	}
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

func TestReasoning_StreamedLiteLLMShapeKeepsOneCopy(t *testing.T) {
	// LiteLLM sends the same reasoning in reasoning_content and reasoning.
	var bodies []map[string]any
	srv := fixedServer(t, &bodies, true, sse(
		`{"choices":[{"delta":{"role":"assistant","reasoning_content":"Think ","reasoning":"Think "}}]}`,
		`{"choices":[{"delta":{"reasoning_content":"hard.","reasoning":"hard."}}]}`,
		`{"choices":[{"delta":{"content":"7"},"finish_reason":"stop"}]}`,
	))
	resp, err := NewAdapter(srv.URL, "k", nil).Complete(context.Background(), CompletionReq{Model: "m", Messages: []Message{{Role: "user", Content: "x"}}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Reasoning != "Think hard." || resp.ReasoningField != "reasoning_content" {
		t.Fatalf("want one copy from reasoning_content, got %q from %q", resp.Reasoning, resp.ReasoningField)
	}
	if resp.Content != "7" {
		t.Fatalf("reasoning must not leak into content, got %q", resp.Content)
	}
}

func TestReasoning_FieldIsRecorded(t *testing.T) {
	for _, field := range []string{"reasoning", "reasoning_text"} {
		t.Run("streamed "+field, func(t *testing.T) {
			var bodies []map[string]any
			srv := fixedServer(t, &bodies, true, sse(
				`{"choices":[{"delta":{"`+field+`":"plan"}}]}`,
				`{"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`,
			))
			resp, err := NewAdapter(srv.URL, "k", nil).Complete(context.Background(), CompletionReq{Model: "m", Messages: []Message{{Role: "user", Content: "x"}}})
			if err != nil {
				t.Fatal(err)
			}
			if resp.Reasoning != "plan" || resp.ReasoningField != field {
				t.Fatalf("want %q from %s, got %q from %q", "plan", field, resp.Reasoning, resp.ReasoningField)
			}
		})
		t.Run("buffered "+field, func(t *testing.T) {
			var bodies []map[string]any
			srv := fixedServer(t, &bodies, false,
				`{"choices":[{"message":{"role":"assistant","content":"ok","`+field+`":"plan"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2}}`)
			resp, err := NewAdapter(srv.URL, "k", nil).Complete(context.Background(), CompletionReq{Model: "m", Messages: []Message{{Role: "user", Content: "x"}}})
			if err != nil {
				t.Fatal(err)
			}
			if resp.Reasoning != "plan" || resp.ReasoningField != field {
				t.Fatalf("want %q from %s, got %q from %q", "plan", field, resp.Reasoning, resp.ReasoningField)
			}
		})
	}
}

func TestReasoning_ReplayedInItsOwnFieldOnlyWhenAsked(t *testing.T) {
	reply := sse(`{"choices":[{"delta":{"content":"done"},"finish_reason":"stop"}]}`)
	history := []Message{
		{Role: "user", Content: "task"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: "bash", Arguments: `{}`}}, Reasoning: "first plan", ReasoningField: "reasoning"},
		{Role: "tool", ToolCallID: "c1", Content: "out"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c2", Name: "bash", Arguments: `{}`}}, Reasoning: "second plan"},
		{Role: "tool", ToolCallID: "c2", Content: "out"},
	}
	var bodies []map[string]any
	a := NewAdapter(fixedServer(t, &bodies, true, reply).URL, "k", nil)
	if _, err := a.Complete(context.Background(), CompletionReq{Model: "m", Messages: history, ReplayReasoning: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Complete(context.Background(), CompletionReq{Model: "m", Messages: history}); err != nil {
		t.Fatal(err)
	}
	msgs := func(i int) []map[string]any {
		var out []map[string]any
		for _, m := range bodies[i]["messages"].([]any) {
			out = append(out, m.(map[string]any))
		}
		return out
	}
	on := msgs(0)
	if on[1]["reasoning"] != "first plan" || on[1]["reasoning_content"] != nil {
		t.Fatalf("reasoning must go back only in the field it came in, got %v", on[1])
	}
	if on[3]["reasoning_content"] != "second plan" || on[3]["reasoning"] != nil {
		t.Fatalf("an unknown field defaults to reasoning_content, got %v", on[3])
	}
	if on[3]["content"] != "" {
		t.Fatalf("reasoning must not be written into content, got %q", on[3]["content"])
	}
	for i, m := range msgs(1) {
		for _, f := range []string{"reasoning_content", "reasoning", "reasoning_text"} {
			if _, ok := m[f]; ok {
				t.Fatalf("replay off must send no reasoning, message %d has %s", i, f)
			}
		}
	}
}

func TestReasoning_AsMessageCarriesIt(t *testing.T) {
	for _, r := range []CompletionResp{
		{Content: "x", Reasoning: "why", ReasoningField: "reasoning"},
		{ToolCalls: []ToolCall{{ID: "1", Name: "t", Arguments: "{}"}}, Reasoning: "why", ReasoningField: "reasoning"},
	} {
		if m := r.AsMessage(); m.Reasoning != "why" || m.ReasoningField != "reasoning" {
			t.Fatalf("AsMessage dropped reasoning: %+v", m)
		}
	}
}
