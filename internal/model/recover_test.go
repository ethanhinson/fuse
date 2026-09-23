package model

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

var recoverTools = []ToolSchema{
	{Name: "read_file", Parameters: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":       map[string]any{"type": "string"},
			"start_line": map[string]any{"type": "integer"},
		},
	}},
	{Name: "write_file", Parameters: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":    map[string]any{"type": "string"},
			"content": map[string]any{"type": "string"},
			"append":  map[string]any{"type": "boolean"},
			"meta":    map[string]any{"type": "object"},
		},
	}},
}

func argsOf(t *testing.T, tc ToolCall) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(tc.Arguments), &m); err != nil {
		t.Fatalf("arguments %q: %v", tc.Arguments, err)
	}
	return m
}

// The shape actually observed: the provider consumed the opening <tool_call>
// and streamed the rest as content, leaving a dangling closer.
func TestRecoverXMLToolCalls_LeakedWithoutOpener(t *testing.T) {
	content := "I'll solve this step by step. First, let me read the task.\n\n" +
		"<function=read_file>\n<parameter=path>\ntask.md\n</parameter>\n</function>\n</tool_call>"
	got, calls, ok := recoverXMLToolCalls(content, recoverTools)
	if !ok || len(calls) != 1 {
		t.Fatalf("recovered = %v, calls = %+v", ok, calls)
	}
	if calls[0].Name != "read_file" || !strings.HasPrefix(calls[0].ID, "call_recovered_") {
		t.Errorf("call = %+v", calls[0])
	}
	if a := argsOf(t, calls[0]); a["path"] != "task.md" {
		t.Errorf("args = %v", a)
	}
	if got != "I'll solve this step by step. First, let me read the task." {
		t.Errorf("content = %q", got)
	}
}

func TestRecoverXMLToolCalls_FullWrapperAndTypedParams(t *testing.T) {
	content := "<tool_call>\n<function=write_file>\n" +
		"<parameter=path>\nsolution.py\n</parameter>\n" +
		"<parameter=content>\nprint(1)\n\n</parameter>\n" + // value keeps its own trailing newline
		"<parameter=append>\ntrue\n</parameter>\n" +
		"<parameter=meta>\n{\"k\": [1, 2]}\n</parameter>\n" +
		"</function>\n</tool_call>\n" +
		"<tool_call>\n<function=read_file>\n<parameter=path>\na.py\n</parameter>\n<parameter=start_line>\n10\n</parameter>\n</function>\n</tool_call>"
	got, calls, ok := recoverXMLToolCalls(content, recoverTools)
	if !ok || len(calls) != 2 {
		t.Fatalf("recovered = %v, calls = %+v", ok, calls)
	}
	w := argsOf(t, calls[0])
	if w["path"] != "solution.py" || w["content"] != "print(1)\n" || w["append"] != true {
		t.Errorf("write args = %v", w)
	}
	if meta, _ := w["meta"].(map[string]any); meta == nil || meta["k"] == nil {
		t.Errorf("meta not parsed as object: %v", w["meta"])
	}
	r := argsOf(t, calls[1])
	if r["path"] != "a.py" || r["start_line"] != float64(10) {
		t.Errorf("read args = %v", r)
	}
	if calls[0].ID == calls[1].ID {
		t.Error("recovered ids must be unique")
	}
	if got != "" {
		t.Errorf("content should be empty after lifting both calls, got %q", got)
	}
}

// A hallucinated tool name is still lifted: the registry's "unknown tool"
// error goes back to the model, exactly as it would had the provider parsed
// the call, instead of the run ending on an apparent final answer.
func TestRecoverXMLToolCalls_UnadvertisedNameIsLiftedUntyped(t *testing.T) {
	content := "Let me just solve it directly:\n\n<function=solve_dp>\n<parameter=n>\n5\n</parameter>\n</function>\n</tool_call>"
	got, calls, ok := recoverXMLToolCalls(content, recoverTools)
	if !ok || len(calls) != 1 || calls[0].Name != "solve_dp" {
		t.Fatalf("recovered = %v, calls = %+v", ok, calls)
	}
	if a := argsOf(t, calls[0]); a["n"] != "5" {
		t.Errorf("unadvertised params should stay strings, got %v", a)
	}
	if got != "Let me just solve it directly:" {
		t.Errorf("content = %q", got)
	}
}

func TestRecoverXMLToolCalls_NonJSONTypedParamStaysText(t *testing.T) {
	content := "<function=read_file>\n<parameter=path>\nx\n</parameter>\n<parameter=start_line>\nten\n</parameter>\n</function>"
	_, calls, ok := recoverXMLToolCalls(content, recoverTools)
	if !ok || len(calls) != 1 {
		t.Fatalf("calls = %+v", calls)
	}
	if a := argsOf(t, calls[0]); a["start_line"] != "ten" {
		t.Errorf("args = %v", a)
	}
}

func TestRecoverXMLToolCalls_LeavesUnrelatedContentAlone(t *testing.T) {
	cases := map[string]struct {
		content string
		tools   []ToolSchema
	}{
		"no tools advertised": {
			"<function=read_file>\n<parameter=path>\nx\n</parameter>\n</function>", nil},
		"prose mentioning the syntax": {
			"Qwen writes calls as <function=NAME> blocks, which is neat.", recoverTools},
		"plain answer": {"The answer is 42.", recoverTools},
	}
	for name, c := range cases {
		got, calls, ok := recoverXMLToolCalls(c.content, c.tools)
		if ok || calls != nil || got != c.content {
			t.Errorf("%s: recovered = %v, calls = %+v, content = %q", name, ok, calls, got)
		}
	}
}

// End to end through the streaming path: content deltas carry the leaked call,
// no tool_calls deltas arrive, and Complete still returns a structured call.
func TestCompleteRecoversLeakedXMLToolCallFromStream(t *testing.T) {
	srv := sseServer(t, []string{
		`{"choices":[{"delta":{"role":"assistant","content":"Let me read the task.\n\n"}}]}`,
		`{"choices":[{"delta":{"content":"<function=read_file>\n<parameter=path>\n"}}]}`,
		`{"choices":[{"delta":{"content":"task.md\n</parameter>\n</function>\n</tool_call>"}}]}`,
		`{"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":45}}`,
	})
	defer srv.Close()

	var trace strings.Builder
	a := NewAdapter(srv.URL, "k", srv.Client()).WithTrace(&trace)
	resp, err := a.Complete(context.Background(), CompletionReq{
		Model: "m", Tools: recoverTools,
		Messages: []Message{{Role: "user", Content: "go"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "read_file" {
		t.Fatalf("tool calls = %+v", resp.ToolCalls)
	}
	if a := argsOf(t, resp.ToolCalls[0]); a["path"] != "task.md" {
		t.Errorf("args = %v", a)
	}
	if resp.Content != "Let me read the task." {
		t.Errorf("content = %q", resp.Content)
	}
	if resp.OutputTokens != 45 {
		t.Errorf("output tokens = %d", resp.OutputTokens)
	}
	if !strings.Contains(trace.String(), "RECOVERED") || !strings.Contains(trace.String(), `"name": "read_file"`) {
		t.Errorf("trace should record the recovered call:\n%s", trace.String())
	}
}

// The buffered (non-streamed) path recovers too, and a response that already
// carries a structured tool call is never touched.
func TestCompleteRecoversLeakedXMLToolCallFromBufferedBody(t *testing.T) {
	srv := jsonServer(t, `{"choices":[{"message":{"role":"assistant",
		"content":"<function=read_file>\n<parameter=path>\ntask.md\n</parameter>\n</function>"}}],
		"usage":{"prompt_tokens":1,"completion_tokens":2}}`)
	defer srv.Close()
	a := NewAdapter(srv.URL, "k", srv.Client())
	resp, err := a.Complete(context.Background(), CompletionReq{Model: "m", Tools: recoverTools})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "read_file" || resp.Content != "" {
		t.Fatalf("resp = %+v", resp)
	}
}

func TestCompleteDoesNotRecoverWhenStructuredCallPresent(t *testing.T) {
	body := `{"choices":[{"message":{"role":"assistant",
		"content":"<function=read_file>\n<parameter=path>\nnope\n</parameter>\n</function>",
		"tool_calls":[{"id":"c1","type":"function","function":{"name":"bash","arguments":"{}"}}]}}]}`
	srv := jsonServer(t, body)
	defer srv.Close()
	a := NewAdapter(srv.URL, "k", srv.Client())
	resp, err := a.Complete(context.Background(), CompletionReq{Model: "m", Tools: recoverTools})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].ID != "c1" || !strings.Contains(resp.Content, "<function=") {
		t.Fatalf("resp = %+v", resp)
	}
}

// jsonServer returns an httptest server that answers every request with the
// given buffered JSON body, exercising the non-streamed reader path.
func jsonServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, body)
	}))
}
