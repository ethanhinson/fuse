package model

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCompleteSendsRequestAndParsesToolCalls(t *testing.T) {
	var gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %q", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{
			"choices":[{"message":{
				"role":"assistant",
				"content":"working on it",
				"tool_calls":[{"id":"call_1","type":"function",
					"function":{"name":"bash","arguments":"{\"command\":\"ls\"}"}}]
			}}]
		}`)
	}))
	defer srv.Close()

	a := NewAdapter(srv.URL, "tkn", srv.Client())
	resp, err := a.Complete(context.Background(), CompletionReq{
		Model:     "cloud/deepseek-v4-flash",
		Messages:  []Message{{Role: "user", Content: "hi"}},
		Tools:     []ToolSchema{{Name: "bash", Description: "run", Parameters: map[string]any{"type": "object"}}},
		MaxTokens: 128,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "working on it" {
		t.Errorf("content = %q", resp.Content)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "bash" {
		t.Fatalf("tool calls = %+v", resp.ToolCalls)
	}
	if resp.ToolCalls[0].Arguments != `{"command":"ls"}` {
		t.Errorf("args = %q", resp.ToolCalls[0].Arguments)
	}
	if gotAuth != "Bearer tkn" {
		t.Errorf("auth = %q", gotAuth)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(gotBody), &sent); err != nil {
		t.Fatal(err)
	}
	if sent["model"] != "cloud/deepseek-v4-flash" {
		t.Errorf("model field = %v", sent["model"])
	}
	if _, ok := sent["tools"]; !ok {
		t.Error("tools field missing from request")
	}
}

func TestCompleteNonOKIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, `{"error":"boom"}`)
	}))
	defer srv.Close()
	a := NewAdapter(srv.URL, "k", srv.Client())
	_, err := a.Complete(context.Background(), CompletionReq{Model: "m", Messages: []Message{{Role: "user", Content: "x"}}})
	if err == nil {
		t.Fatal("expected error on 500")
	}
}

func TestAsMessage(t *testing.T) {
	r := CompletionResp{Content: "done", ToolCalls: []ToolCall{{ID: "c1", Name: "bash", Arguments: "{}"}}}
	m := r.AsMessage()
	if m.Role != "assistant" || m.Content != "done" || len(m.ToolCalls) != 1 {
		t.Fatalf("as message = %+v", m)
	}
}
