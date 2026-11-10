package model

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

var wireOK = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

func toolsNamed(names ...string) []ToolSchema {
	var out []ToolSchema
	for _, n := range names {
		out = append(out, ToolSchema{Name: n, Description: "d", Parameters: map[string]any{"type": "object"}})
	}
	return out
}

func TestWireNames_EncodeAndBuiltinsUnchanged(t *testing.T) {
	w := newWireNames(toolsNamed("read_file", "bash", "mcp:playwright/browser_navigate"))
	if got := w.wire("read_file"); got != "read_file" {
		t.Fatalf("built-in changed: %q", got)
	}
	if got := w.wire("mcp:playwright/browser_navigate"); got != "mcp__playwright__browser_navigate" {
		t.Fatalf("got %q", got)
	}
	if got := w.actual("mcp__playwright__browser_navigate"); got != "mcp:playwright/browser_navigate" {
		t.Fatalf("decode got %q", got)
	}
	if got := w.actual("nope"); got != "nope" {
		t.Fatalf("unknown wire name must pass through, got %q", got)
	}
}

func TestWireNames_LongAndCollisionUnique(t *testing.T) {
	long1 := "mcp:" + strings.Repeat("s", 40) + "/" + strings.Repeat("t", 40)
	long2 := "mcp:" + strings.Repeat("s", 40) + "/" + strings.Repeat("t", 39) + "u"
	names := []string{"mcp:a/b", "mcp__a__b", "mcp:a:b", "mcp:a/b?", "mcp:a/b!", long1, long2}
	w := newWireNames(toolsNamed(names...))
	seen := map[string]string{}
	for _, n := range names {
		g := w.wire(n)
		if !wireOK.MatchString(g) {
			t.Fatalf("%q -> %q not provider safe", n, g)
		}
		if prev, dup := seen[g]; dup {
			t.Fatalf("%q and %q both -> %q", prev, n, g)
		}
		seen[g] = n
		if w.actual(g) != n {
			t.Fatalf("round trip %q -> %q -> %q", n, g, w.actual(g))
		}
	}
	if w.wire("mcp__a__b") != "mcp__a__b" {
		t.Fatalf("already-safe name must be untouched")
	}
	w2 := newWireNames(toolsNamed(names...))
	for _, n := range names {
		if w.wire(n) != w2.wire(n) {
			t.Fatalf("unstable encoding for %q", n)
		}
	}
}

func wireToolNames(body map[string]any) []string {
	var out []string
	for _, t := range body["tools"].([]any) {
		out = append(out, t.(map[string]any)["function"].(map[string]any)["name"].(string))
	}
	return out
}

func TestWireNames_BufferedRoundTripAndReplay(t *testing.T) {
	var bodies []map[string]any
	srv := fixedServer(t, &bodies, false, `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"mcp__pw__nav","arguments":"{}"}},{"id":"c2","type":"function","function":{"name":"ghost_tool","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
	a := NewAdapter(srv.URL, "k", nil)
	tools := toolsNamed("read_file", "mcp:pw/nav")
	msgs := []Message{
		{Role: "user", Content: "go"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "p1", Name: "mcp:pw/nav", Arguments: "{}"}}},
		{Role: "tool", ToolCallID: "p1", Name: "mcp:pw/nav", Content: "ok"},
	}
	resp, err := a.Complete(context.Background(), CompletionReq{Model: "m", Messages: msgs, Tools: tools})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ToolCalls) != 2 || resp.ToolCalls[0].Name != "mcp:pw/nav" || resp.ToolCalls[1].Name != "ghost_tool" {
		t.Fatalf("decode: %+v", resp.ToolCalls)
	}
	got := wireToolNames(bodies[0])
	if got[0] != "read_file" || got[1] != "mcp__pw__nav" {
		t.Fatalf("tools on wire: %v", got)
	}
	raw, _ := json.Marshal(bodies[0]["messages"])
	s := string(raw)
	if strings.Contains(s, "mcp:pw/nav") || strings.Count(s, "mcp__pw__nav") != 2 {
		t.Fatalf("history not re-encoded consistently: %s", s)
	}
	if _, err := a.Complete(context.Background(), CompletionReq{Model: "m", Messages: msgs, Tools: tools}); err != nil {
		t.Fatal(err)
	}
	b1, _ := json.Marshal(bodies[0]["tools"])
	b2, _ := json.Marshal(bodies[1]["tools"])
	if string(b1) != string(b2) {
		t.Fatalf("tool list not stable")
	}
}

func TestWireNames_StreamedDeltasDecode(t *testing.T) {
	var bodies []map[string]any
	srv := fixedServer(t, &bodies, true, sse(
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"mcp__pw__nav","arguments":"{\"u"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"rl\":1}"}}]},"finish_reason":"tool_calls"}]}`,
	))
	resp, err := NewAdapter(srv.URL, "k", nil).Complete(context.Background(), CompletionReq{
		Model: "m", Messages: []Message{{Role: "user", Content: "x"}}, Tools: toolsNamed("mcp:pw/nav"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "mcp:pw/nav" || resp.ToolCalls[0].Arguments != `{"url":1}` {
		t.Fatalf("got %+v", resp.ToolCalls)
	}
}
