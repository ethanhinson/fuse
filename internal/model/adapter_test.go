package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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
	a.RetryBackoff = 0
	_, err := a.Complete(context.Background(), CompletionReq{Model: "m", Messages: []Message{{Role: "user", Content: "x"}}})
	if err == nil {
		t.Fatal("expected error on 500")
	}
}

const okCompletion = `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`

func TestCompleteRetriesTransientFailures(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, okCompletion)
	}))
	defer srv.Close()

	a := NewAdapter(srv.URL, "k", srv.Client())
	a.RetryBackoff = 0
	resp, err := a.Complete(context.Background(), CompletionReq{Model: "m", Messages: []Message{{Role: "user", Content: "x"}}})
	if err != nil {
		t.Fatalf("expected success after retries, got %v", err)
	}
	if resp.Content != "ok" {
		t.Errorf("content = %q", resp.Content)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("server calls = %d, want 3", got)
	}
}

func TestCompleteNoRetryOn400(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	a := NewAdapter(srv.URL, "k", srv.Client())
	a.RetryBackoff = 0
	if _, err := a.Complete(context.Background(), CompletionReq{Model: "m"}); err == nil {
		t.Fatal("expected error on 400")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("server calls = %d, want 1 (400 must not retry)", got)
	}
}

func TestCompletePerAttemptTimeout(t *testing.T) {
	var calls atomic.Int32
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		io.Copy(io.Discard, r.Body) // drain so disconnect detection works
		select {                    // hang until the test ends or the client gives up
		case <-block:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(block) // LIFO: unblock handlers BEFORE srv.Close waits on them

	a := NewAdapter(srv.URL, "k", srv.Client())
	a.RequestTimeout = 30 * time.Millisecond
	a.MaxAttempts = 2
	a.RetryBackoff = 0

	start := time.Now()
	_, err := a.Complete(context.Background(), CompletionReq{Model: "m"})
	if err == nil {
		t.Fatal("expected timeout error from hung server")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("took %v; per-attempt timeout not applied", elapsed)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("server calls = %d, want 2 (timeout should retry once)", got)
	}
}

func TestCompleteParentCancelAbortsWithoutRetry(t *testing.T) {
	var calls atomic.Int32
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		io.Copy(io.Discard, r.Body)
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(block) // LIFO: unblock handlers BEFORE srv.Close waits on them

	a := NewAdapter(srv.URL, "k", srv.Client())
	a.RetryBackoff = 0
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_, err := a.Complete(ctx, CompletionReq{Model: "m"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("server calls = %d, want 1 (cancel must not retry)", got)
	}
}

// TestFailureErrorCarriesDiagnostics: a timeout/failure error must identify
// the model, payload size, attempt count, and duration — a bare transport
// error is undiagnosable after the fact.
func TestFailureErrorCarriesDiagnostics(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	var trace bytes.Buffer
	a := NewAdapter(srv.URL, "k", srv.Client()).WithTraceLabel(&trace, "worker-3")
	a.RetryBackoff = 0
	_, err := a.Complete(context.Background(), CompletionReq{
		Model:    "cloud/deepseek-v4-flash",
		Messages: []Message{{Role: "user", Content: strings.Repeat("x", 10_000)}},
	})
	if err == nil {
		t.Fatal("expected failure")
	}
	msg := err.Error()
	for _, want := range []string{"cloud/deepseek-v4-flash", "3 attempt(s)", "payload", "KB"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error missing %q: %s", want, msg)
		}
	}
	out := trace.String()
	if !strings.Contains(out, "── ERROR [worker-3] ──") {
		t.Error("trace missing labeled ERROR block")
	}
	if !strings.Contains(out, "── RETRY [worker-3] ──") || !strings.Contains(out, "model=cloud/deepseek-v4-flash") {
		t.Error("trace RETRY blocks missing model diagnostics")
	}
}

func TestTraceLabelAppearsInMarkers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, okCompletion)
	}))
	defer srv.Close()

	var buf bytes.Buffer
	a := NewAdapter(srv.URL, "k", srv.Client()).WithTraceLabel(&buf, "read-agent")
	if _, err := a.Complete(context.Background(), CompletionReq{Model: "m"}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "── REQ [read-agent] ──") || !strings.Contains(out, "── RESP [read-agent] ──") {
		t.Errorf("trace missing labeled markers:\n%s", out)
	}
}

func TestAsMessage(t *testing.T) {
	r := CompletionResp{Content: "done", ToolCalls: []ToolCall{{ID: "c1", Name: "bash", Arguments: "{}"}}}
	m := r.AsMessage()
	if m.Role != "assistant" || m.Content != "done" || len(m.ToolCalls) != 1 {
		t.Fatalf("as message = %+v", m)
	}
}
