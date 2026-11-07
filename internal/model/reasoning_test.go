package model

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// reasoningServer records each request body and streams a reply whose usage
// reports a reasoning share.
func reasoningServer(t *testing.T, bodies *[]map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		*bodies = append(*bodies, body)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"hi"},"finish_reason":"stop"}]}`+"\n\n")
		fmt.Fprint(w, `data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":100,"completion_tokens_details":{"reasoning_tokens":90}}}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestReasoningEffort_SentOnlyWhenSet(t *testing.T) {
	var bodies []map[string]any
	a := NewAdapter(reasoningServer(t, &bodies).URL, "k", nil)
	msgs := []Message{{Role: "user", Content: "x"}}
	if _, err := a.Complete(context.Background(), CompletionReq{Model: "m", Messages: msgs, ReasoningEffort: "low"}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Complete(context.Background(), CompletionReq{Model: "m", Messages: msgs}); err != nil {
		t.Fatal(err)
	}
	if got := bodies[0]["reasoning_effort"]; got != "low" {
		t.Fatalf("want reasoning_effort low on the wire, got %v", got)
	}
	if _, present := bodies[1]["reasoning_effort"]; present {
		t.Fatal("an unset effort must send nothing, leaving the provider's default")
	}
}

func TestReasoningTokens_ReadFromStreamedUsage(t *testing.T) {
	var bodies []map[string]any
	a := NewAdapter(reasoningServer(t, &bodies).URL, "k", nil)
	resp, err := a.Complete(context.Background(), CompletionReq{Model: "m", Messages: []Message{{Role: "user", Content: "x"}}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.ReasoningTokens != 90 || resp.OutputTokens != 100 {
		t.Fatalf("want 90 of 100 output tokens as reasoning, got %d of %d", resp.ReasoningTokens, resp.OutputTokens)
	}
}
