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

// sseServer returns an httptest server that streams the given SSE lines with a
// text/event-stream content type, flushing after each so the client sees
// headers (and the first byte) immediately — this is what makes time-to-first-
// byte, not time-to-full-generation, the header-timeout metric.
func sseServer(t *testing.T, chunks []string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		if fl != nil {
			fl.Flush() // send headers before any data
		}
		for _, c := range chunks {
			io.WriteString(w, "data: "+c+"\n\n")
			if fl != nil {
				fl.Flush()
			}
		}
		io.WriteString(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
	}))
}

// TestCompleteRequestsStreaming: the request body must ask for streaming so
// LiteLLM does not buffer the whole (possibly minutes-long) generation before
// sending any headers.
func TestCompleteRequestsStreaming(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, okCompletion)
	}))
	defer srv.Close()

	a := NewAdapter(srv.URL, "k", srv.Client())
	if _, err := a.Complete(context.Background(), CompletionReq{Model: "m"}); err != nil {
		t.Fatal(err)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(gotBody), &sent); err != nil {
		t.Fatal(err)
	}
	if sent["stream"] != true {
		t.Errorf("request must set stream:true, got %v", sent["stream"])
	}
}

// TestCompleteParsesSSEStream: a text/event-stream response is reassembled into
// content plus tool calls, with tool-call arguments fragmented across chunks
// (as real providers stream them) concatenated in order, and usage read from
// the final chunk.
func TestCompleteParsesSSEStream(t *testing.T) {
	srv := sseServer(t, []string{
		`{"choices":[{"delta":{"role":"assistant","content":"Hello"}}]}`,
		`{"choices":[{"delta":{"content":", world"}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"web_search","arguments":"{\"query\":"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"litestream\"}"}}]}}]}`,
		`{"choices":[{"delta":{}}],"usage":{"prompt_tokens":11,"completion_tokens":22}}`,
	})
	defer srv.Close()

	a := NewAdapter(srv.URL, "k", srv.Client())
	resp, err := a.Complete(context.Background(), CompletionReq{Model: "m", Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "Hello, world" {
		t.Errorf("content = %q, want %q", resp.Content, "Hello, world")
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("tool calls = %+v, want 1", resp.ToolCalls)
	}
	tc := resp.ToolCalls[0]
	if tc.ID != "call_1" || tc.Name != "web_search" || tc.Arguments != `{"query":"litestream"}` {
		t.Errorf("reassembled tool call = %+v", tc)
	}
	if resp.InputTokens != 11 || resp.OutputTokens != 22 {
		t.Errorf("usage = in %d out %d, want 11/22", resp.InputTokens, resp.OutputTokens)
	}
}

// TestCompleteSSEMultipleToolCalls: parallel tool calls stream interleaved by
// index and must reassemble into distinct calls in index order.
func TestCompleteSSEMultipleToolCalls(t *testing.T) {
	srv := sseServer(t, []string{
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","type":"function","function":{"name":"f0","arguments":"{}"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"b","type":"function","function":{"name":"f1","arguments":"{\"x\":1}"}}]}}]}`,
	})
	defer srv.Close()

	a := NewAdapter(srv.URL, "k", srv.Client())
	resp, err := a.Complete(context.Background(), CompletionReq{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ToolCalls) != 2 {
		t.Fatalf("want 2 tool calls, got %+v", resp.ToolCalls)
	}
	if resp.ToolCalls[0].Name != "f0" || resp.ToolCalls[1].Name != "f1" {
		t.Errorf("tool calls out of order: %+v", resp.ToolCalls)
	}
	if resp.ToolCalls[1].Arguments != `{"x":1}` {
		t.Errorf("second call args = %q", resp.ToolCalls[1].Arguments)
	}
}

// TestCompleteSSETracesReassembled: the trace must still record a RESP block for
// a streamed response, so --trace stays useful with streaming on.
func TestCompleteSSETracesReassembled(t *testing.T) {
	srv := sseServer(t, []string{
		`{"choices":[{"delta":{"content":"streamed reply"}}]}`,
	})
	defer srv.Close()

	var buf bytes.Buffer
	a := NewAdapter(srv.URL, "k", srv.Client()).WithTraceLabel(&buf, "root")
	if _, err := a.Complete(context.Background(), CompletionReq{Model: "m"}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "── RESP [root] ──") {
		t.Errorf("streamed trace missing RESP block:\n%s", out)
	}
	if !strings.Contains(out, "streamed reply") {
		t.Errorf("streamed trace missing reassembled content:\n%s", out)
	}
}

// TestCompleteSSEErrorEventIsError: an error object streamed as an SSE data
// event (LiteLLM's mid-stream error shape) surfaces as an error, not a silent
// empty completion.
func TestCompleteSSEErrorEventIsError(t *testing.T) {
	srv := sseServer(t, []string{
		`{"error":{"message":"upstream exploded","type":"server_error"}}`,
	})
	defer srv.Close()

	a := NewAdapter(srv.URL, "k", srv.Client())
	a.MaxAttempts = 1
	_, err := a.Complete(context.Background(), CompletionReq{Model: "m"})
	if err == nil {
		t.Fatal("expected an error from a streamed error event")
	}
	if !strings.Contains(err.Error(), "upstream exploded") {
		t.Errorf("error should carry the streamed message: %v", err)
	}
}

// TestCompleteSSETransientErrorRetries: a provider dropping the connection
// mid-generation reaches us as a streamed error event (LiteLLM's
// "provider_unavailable" shape); that is transient and must be retried.
func TestCompleteSSETransientErrorRetries(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			io.WriteString(w, `data: {"error":{"message":"litellm.APIError: APIError: OpenrouterException - Message: Network connection lost., Metadata: {'error_type': 'provider_unavailable'}","type":"None","code":"500"}}`+"\n\n")
			return
		}
		io.WriteString(w, `data: {"choices":[{"delta":{"role":"assistant","content":"recovered"}}]}`+"\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	a := NewAdapter(srv.URL, "k", srv.Client())
	a.MaxAttempts = 2
	a.RetryBackoff = time.Millisecond
	resp, err := a.Complete(context.Background(), CompletionReq{Model: "m"})
	if err != nil {
		t.Fatalf("transient streamed error should have been retried: %v", err)
	}
	if resp.Content != "recovered" || atomic.LoadInt32(&calls) != 2 {
		t.Errorf("want content from the second attempt (2 calls), got %q after %d calls", resp.Content, calls)
	}
}

// TestCompleteSSEClientErrorNotRetried: a streamed 4xx rejection is terminal;
// re-sending the same request would only fail the same way.
func TestCompleteSSEClientErrorNotRetried(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"error":{"message":"litellm.BadRequestError: context length exceeded","type":"invalid_request_error","code":"400"}}`+"\n\n")
	}))
	defer srv.Close()

	a := NewAdapter(srv.URL, "k", srv.Client())
	a.MaxAttempts = 3
	a.RetryBackoff = time.Millisecond
	if _, err := a.Complete(context.Background(), CompletionReq{Model: "m"}); err == nil {
		t.Fatal("expected the streamed 400 to surface as an error")
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("a client error must not be retried: %d calls", n)
	}
}

// TestFinishReasonParsedBufferedAndStreamed: the choice-level finish_reason
// reaches CompletionResp on both reader paths, so the loop can tell a reply
// that was cut at max_tokens from one that finished.
func TestFinishReasonParsedBufferedAndStreamed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("stream") == "1" {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, `data: {"choices":[{"delta":{"role":"assistant","content":"partial"},"finish_reason":null}]}`+"\n\n")
			io.WriteString(w, `data: {"choices":[{"delta":{},"finish_reason":"length"}],"usage":{"prompt_tokens":3,"completion_tokens":100}}`+"\n\n")
			io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"partial"},"finish_reason":"length"}],"usage":{"prompt_tokens":3,"completion_tokens":100}}`)
	}))
	defer srv.Close()
	for _, mode := range []string{"", "?stream=1"} {
		a := NewAdapter(srv.URL+mode, "k", srv.Client())
		resp, err := a.Complete(context.Background(), CompletionReq{Model: "m", Messages: []Message{{Role: "user", Content: "hi"}}, MaxTokens: 100})
		if err != nil {
			t.Fatalf("mode %q: %v", mode, err)
		}
		if resp.FinishReason != "length" {
			t.Errorf("mode %q: finish=%q", mode, resp.FinishReason)
		}
	}
}

// TestCachedTokensParsed: a provider's prompt_tokens_details.cached_tokens
// reaches CompletionResp on both reader paths and survives into the
// reassembled trace block, so a run can measure its prefix-cache hit rate.
func TestCachedTokensParsed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("stream") == "1" {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, `data: {"choices":[{"delta":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4024,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":3168,"audio_tokens":0}}}`+"\n\n")
			io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4024,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":3168}}}`)
	}))
	defer srv.Close()
	for _, mode := range []string{"", "?stream=1"} {
		var trace strings.Builder
		a := NewAdapter(srv.URL+mode, "k", srv.Client()).WithTrace(&trace)
		resp, err := a.Complete(context.Background(), CompletionReq{Model: "m", Messages: []Message{{Role: "user", Content: "hi"}}})
		if err != nil {
			t.Fatalf("mode %q: %v", mode, err)
		}
		if resp.InputTokens != 4024 || resp.CachedTokens != 3168 {
			t.Errorf("mode %q: input=%d cached=%d", mode, resp.InputTokens, resp.CachedTokens)
		}
		if !strings.Contains(trace.String(), `"cached_tokens": 3168`) {
			t.Errorf("mode %q: trace lacks cached_tokens: %s", mode, trace.String())
		}
	}
}

// TestAsMessageWrapsMalformedArguments: a tool call whose arguments are not
// JSON keeps the call for execution (the registry will answer "bad
// arguments") but the history copy is a valid JSON object, so a provider that
// validates the transcript does not reject every later request. A call-free
// reply keeps a nil ToolCalls slice (transcript round-trips compare deeply).
func TestAsMessageWrapsMalformedArguments(t *testing.T) {
	r := CompletionResp{ToolCalls: []ToolCall{{ID: "1", Name: "write_file", Arguments: `{"path": "x"`}, {ID: "2", Name: "bash", Arguments: `{"command":"ls"}`}}}
	m := r.AsMessage()
	if !json.Valid([]byte(m.ToolCalls[0].Arguments)) || !strings.Contains(m.ToolCalls[0].Arguments, `{\"path\": \"x\"`) {
		t.Errorf("malformed args not wrapped: %s", m.ToolCalls[0].Arguments)
	}
	if m.ToolCalls[1].Arguments != `{"command":"ls"}` || r.ToolCalls[0].Arguments != `{"path": "x"` {
		t.Errorf("valid args or the response itself changed")
	}
	if got := (CompletionResp{Content: "done"}).AsMessage(); got.ToolCalls != nil {
		t.Errorf("call-free reply should keep a nil slice")
	}
}
