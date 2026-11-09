package model

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// These tests pin the wire bytes, trace text and parsed replies of every
// request path against golden files, so the dialect refactor can be shown not to
// change behaviour. Regenerate only deliberately:
//
//	go test ./internal/model -run DialectEquiv -update-dialect-equiv
var updateDialectEquiv = flag.Bool("update-dialect-equiv", false, "rewrite internal/model/testdata/dialect_equiv goldens")

var (
	recoveredID = regexp.MustCompile(`call_recovered_[0-9a-f]{16}`)
	hostPort    = regexp.MustCompile(`127\.0\.0\.1:[0-9]+`)
)

func normalizeEquiv(s string) string {
	s = recoveredID.ReplaceAllString(s, "call_recovered_X")
	return hostPort.ReplaceAllString(s, "HOST")
}

// rawServer records every request body as raw bytes and serves replies in
// order (the last one repeats). A reply starting with "data:" is an SSE stream,
// one starting with "HTTP <code> " is that status with the rest as its body,
// anything else is a JSON body.
func rawServer(t *testing.T, replies ...string) (*httptest.Server, *[][]byte) {
	t.Helper()
	var mu sync.Mutex
	var bodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		n := len(bodies)
		bodies = append(bodies, b)
		mu.Unlock()
		if n >= len(replies) {
			n = len(replies) - 1
		}
		reply := replies[n]
		status := 200
		if rest, ok := strings.CutPrefix(reply, "HTTP "); ok {
			var code int
			fmt.Sscanf(rest, "%d", &code)
			status = code
			reply = rest[strings.Index(rest, " ")+1:]
		}
		if strings.HasPrefix(reply, "data:") {
			w.Header().Set("Content-Type", "text/event-stream")
		} else {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(status)
		fmt.Fprint(w, reply)
	}))
	t.Cleanup(srv.Close)
	return srv, &bodies
}

func equivPath(t *testing.T) string {
	t.Helper()
	return filepath.Join("testdata", "dialect_equiv", t.Name()+".golden")
}

func compareEquiv(t *testing.T, got string) {
	t.Helper()
	got = normalizeEquiv(got)
	path := equivPath(t)
	if *updateDialectEquiv {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run with -update-dialect-equiv to create it): %v", err)
	}
	if string(want) != got {
		t.Errorf("%s differs from golden\n--- want\n%s\n--- got\n%s", path, want, got)
	}
}

var equivPNG = []byte{0x89, 'P', 'N', 'G', 1, 2, 3, 4}

func equivTools() []ToolSchema {
	return []ToolSchema{
		{Name: "read_file", Description: "read a file", Parameters: map[string]any{
			"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}},
		}},
		{Name: "write_file", Description: "write a file", Parameters: map[string]any{
			"type": "object", "properties": map[string]any{
				"path": map[string]any{"type": "string"}, "content": map[string]any{"type": "string"},
			},
		}},
	}
}

func kitchenSink() []Message {
	return []Message{
		{Role: "system", Content: "be useful"},
		{Role: "user", Content: "look at these"},
		{Role: "user", Content: "with images", Attachments: []Attachment{
			{MediaType: "image/png", Data: equivPNG, Name: "a.png"},
			{MediaType: "image/png", Name: "gone.png"},
		}},
		{Role: "assistant", Reasoning: "think rc", ReasoningField: "reasoning_content",
			ToolCalls: []ToolCall{{ID: "c1", Name: "read_file", Arguments: `{"path":"a.go"}`}}},
		{Role: "tool", ToolCallID: "c1", Name: "read_file", Content: "package a"},
		{Role: "assistant", Content: "noted", Reasoning: "think r", ReasoningField: "reasoning"},
		{Role: "tool", ToolCallID: "c1", Name: "read_file", Content: ""},
		{Role: "assistant", Content: "again", Reasoning: "think rt", ReasoningField: "reasoning_text"},
		{Role: "assistant", Content: "default", Reasoning: "think none"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c2", Name: "write_file", Arguments: ""}}},
		{Role: "user", Content: "carry on"},
	}
}

func emptyHistory() []Message {
	return []Message{
		{Role: "system", Content: "be useful"},
		{Role: "user", Content: ""},
		{Role: "assistant", Content: "x"},
		{Role: "tool", ToolCallID: "c", Name: "n", Content: ""},
	}
}

const okReply = `data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n"

func runEquiv(t *testing.T, req CompletionReq, replies ...string) string {
	t.Helper()
	srv, bodies := rawServer(t, replies...)
	var trace bytes.Buffer
	a := NewAdapter(srv.URL, "k", nil).WithTrace(&trace)
	a.MaxAttempts = 1
	a.RetryBackoff = 0
	resp, err := a.Complete(context.Background(), req)
	var out strings.Builder
	if err != nil {
		fmt.Fprintf(&out, "ERROR: %v\n", err)
	} else {
		j, _ := json.MarshalIndent(resp, "", "  ")
		fmt.Fprintf(&out, "RESPONSE:\n%s\n", j)
	}
	fmt.Fprintf(&out, "REQUESTS: %d\n", len(*bodies))
	for i, b := range *bodies {
		fmt.Fprintf(&out, "BODY %d:\n%s\n", i, b)
	}
	fmt.Fprintf(&out, "TRACE:\n%s\n", trace.String())
	return out.String()
}

var equivIDs = []string{
	"cloud/claude-haiku-5.5", "openrouter/anthropic/claude-sonnet-5",
	"cloud/deepseek-v4-flash", "my-proxy/alias", "local/qwen3-coder",
}

var equivDialects = []string{"", "openai", "anthropic", "bogus"}

func equivName(s string) string {
	if s == "" {
		return "unset"
	}
	return strings.NewReplacer("/", "_", ".", "_").Replace(s)
}

func TestDialectEquivWire(t *testing.T) {
	for _, id := range equivIDs {
		for _, d := range equivDialects {
			t.Run(equivName(id)+"__"+equivName(d), func(t *testing.T) {
				compareEquiv(t, runEquiv(t, CompletionReq{
					Model: id, Dialect: d, Messages: kitchenSink(), Tools: equivTools(),
					ReplayReasoning: true, Attachments: true, MaxTokens: 4096, ReasoningEffort: "high",
				}, okReply))
			})
		}
	}
	for _, id := range []string{"cloud/claude-haiku-5.5", "cloud/deepseek-v4-flash"} {
		n := equivName(id)
		t.Run(n+"__no_replay", func(t *testing.T) {
			compareEquiv(t, runEquiv(t, CompletionReq{Model: id, Messages: kitchenSink(), Tools: equivTools(),
				Attachments: true, MaxTokens: 4096}, okReply))
		})
		t.Run(n+"__no_attachments", func(t *testing.T) {
			compareEquiv(t, runEquiv(t, CompletionReq{Model: id, Messages: kitchenSink(), Tools: equivTools(),
				ReplayReasoning: true, MaxTokens: 4096}, okReply))
		})
		t.Run(n+"__tool_choice_none", func(t *testing.T) {
			compareEquiv(t, runEquiv(t, CompletionReq{Model: id, Messages: kitchenSink(),
				ReplayReasoning: true, Attachments: true, ToolChoice: "none"}, okReply))
		})
		t.Run(n+"__empty_history", func(t *testing.T) {
			compareEquiv(t, runEquiv(t, CompletionReq{Model: id, Messages: emptyHistory(), Tools: equivTools()}, okReply))
		})
	}
}

func chunk(delta, finish string) string {
	if finish != "" {
		finish = `,"finish_reason":"` + finish + `"`
	}
	return `{"choices":[{"delta":` + delta + finish + `}]}`
}

const xmlCall = "<function=read_file>\n<parameter=path>\ntask.md\n</parameter>\n</function>\n</tool_call>"

func TestDialectEquivResponse(t *testing.T) {
	type fixture struct {
		name    string
		noTools bool
		replies []string
	}
	fixtures := []fixture{
		{name: "reasoning_content", replies: []string{sse(chunk(`{"reasoning_content":"Think "}`, ""), chunk(`{"reasoning_content":"hard."}`, ""), chunk(`{"content":"7"}`, "stop"))}},
		{name: "reasoning", replies: []string{sse(chunk(`{"reasoning":"Think "}`, ""), chunk(`{"reasoning":"hard."}`, ""), chunk(`{"content":"7"}`, "stop"))}},
		{name: "reasoning_text", replies: []string{sse(chunk(`{"reasoning_text":"Think "}`, ""), chunk(`{"reasoning_text":"hard."}`, ""), chunk(`{"content":"7"}`, "stop"))}},
		{name: "litellm_duplicate", replies: []string{sse(chunk(`{"reasoning_content":"Think ","reasoning":"Think "}`, ""), chunk(`{"reasoning_content":"hard.","reasoning":"hard."}`, ""), chunk(`{"content":"7"}`, "stop"))}},
		{name: "xml_streamed", replies: []string{sse(chunk(`{"content":`+jsonStr("Reading.\n\n"+xmlCall)+`}`, "stop"))}},
		{name: "xml_buffered", replies: []string{`{"choices":[{"message":{"role":"assistant","content":` + jsonStr("Reading.\n\n"+xmlCall) + `},"finish_reason":"stop"}]}`}},
		{name: "xml_no_tools", noTools: true, replies: []string{sse(chunk(`{"content":`+jsonStr("Reading.\n\n"+xmlCall)+`}`, "stop"))}},
		{name: "structured_plus_xml", replies: []string{sse(
			chunk(`{"content":`+jsonStr("Also.\n\n"+xmlCall)+`,"tool_calls":[{"index":0,"id":"c9","type":"function","function":{"name":"write_file","arguments":"{\"path\":\"x\"}"}}]}`, "tool_calls"))}},
		{name: "empty_then_full", replies: []string{
			sse(chunk(`{"reasoning_content":"hm"}`, "stop")),
			`{"choices":[{"message":{"role":"assistant","content":"full answer","reasoning_content":"hm"},"finish_reason":"stop"}]}`}},
		{name: "empty_then_empty", replies: []string{
			sse(chunk(`{"reasoning_content":"hm"}`, "stop")),
			`{"choices":[{"message":{"role":"assistant","content":""},"finish_reason":"stop"}]}`}},
		{name: "empty_then_500", replies: []string{
			sse(chunk(`{"reasoning_content":"hm"}`, "stop")),
			`HTTP 500 {"error":"boom"}`}},
		{name: "empty_length_no_retry", replies: []string{sse(chunk(`{"reasoning_content":"hm"}`, "length"))}},
		{name: "stream_error_event", replies: []string{`data: {"error":{"message":"bad request","code":400}}` + "\n\ndata: [DONE]\n\n"}},
	}
	for _, id := range []string{"cloud/claude-haiku-5.5", "cloud/deepseek-v4-flash"} {
		for _, f := range fixtures {
			t.Run(equivName(id)+"__"+f.name, func(t *testing.T) {
				req := CompletionReq{Model: id, Messages: agentHistory(), ReplayReasoning: true, MaxTokens: 1024}
				if !f.noTools {
					req.Tools = equivTools()
				}
				compareEquiv(t, runEquiv(t, req, f.replies...))
			})
		}
	}
}

func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestDialectEquivAcceptsAttachments(t *testing.T) {
	yes, no := true, false
	var out strings.Builder
	for _, id := range append([]string{"cli/claude-max"}, equivIDs...) {
		for _, d := range equivDialects {
			for _, att := range []*bool{nil, &yes, &no} {
				for _, tags := range [][]string{nil, {"vision"}} {
					mc := ModelConfig{ID: id, Dialect: d, Attachments: att, Tags: tags}
					a := "nil"
					if att != nil {
						a = fmt.Sprint(*att)
					}
					fmt.Fprintf(&out, "id=%s dialect=%q attachments=%s tags=%v => %v\n", id, d, a, tags, mc.AcceptsAttachments())
				}
			}
		}
	}
	compareEquiv(t, out.String())
}
