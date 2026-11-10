package model

import (
	"context"
	"strings"
	"testing"
)

func TestWireNames_IndependentOfOrder(t *testing.T) {
	a := newWireNames(toolsNamed("mcp:a/b", "mcp:a:b", "mcp__a__b", "mcp:x/y"), nil)
	b := newWireNames(toolsNamed("mcp:x/y", "mcp__a__b", "mcp:a:b", "mcp:a/b"), nil)
	for _, n := range []string{"mcp:a/b", "mcp:a:b", "mcp__a__b", "mcp:x/y"} {
		if a.wire(n) != b.wire(n) {
			t.Fatalf("%q depends on order: %q vs %q", n, a.wire(n), b.wire(n))
		}
	}
	// Collision with a safe name and among unsafe names hashes the unsafe ones.
	if a.wire("mcp__a__b") != "mcp__a__b" || a.wire("mcp:a/b") == "mcp__a__b" || a.wire("mcp:a/b") == a.wire("mcp:a:b") {
		t.Fatalf("collisions not resolved: %q %q %q", a.wire("mcp__a__b"), a.wire("mcp:a/b"), a.wire("mcp:a:b"))
	}
	// A non-colliding name stays plain.
	if a.wire("mcp:x/y") != "mcp__x__y" {
		t.Fatalf("plain encoding lost: %q", a.wire("mcp:x/y"))
	}
}

func TestWireNames_HistoryNamesSeedMapping(t *testing.T) {
	hist := []Message{{Role: "assistant", ToolCalls: []ToolCall{{ID: "1", Name: "mcp:a/b"}}}}
	w := newWireNames(toolsNamed("mcp:a:b"), hist)
	if w.wire("mcp:a/b") == w.wire("mcp:a:b") {
		t.Fatal("history name collides with advertised name")
	}
	if w.actual(w.wire("mcp:a/b")) != "mcp:a/b" {
		t.Fatal("history name does not decode")
	}
}

func TestWireNames_XMLRecoveryDecodesEncodedName(t *testing.T) {
	var bodies []map[string]any
	call := "<function=mcp__pw__nav>\n<parameter=url>\nhttp://x\n</parameter>\n</function>\n</tool_call>"
	srv := fixedServer(t, &bodies, false,
		`{"choices":[{"message":{"role":"assistant","content":`+jsonStr("Going.\n\n"+call)+`},"finish_reason":"stop"}]}`)
	resp, err := NewAdapter(srv.URL, "k", nil).Complete(context.Background(), CompletionReq{
		Model: "m", Messages: []Message{{Role: "user", Content: "x"}}, Tools: toolsNamed("mcp:pw/nav"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "mcp:pw/nav" {
		t.Fatalf("recovered call = %+v", resp.ToolCalls)
	}
	if !strings.Contains(resp.ToolCalls[0].Arguments, "http://x") {
		t.Fatalf("args lost: %q", resp.ToolCalls[0].Arguments)
	}
}

func TestResolveToolName(t *testing.T) {
	adv := []string{"bash", "mcp:srv/tool"}
	for in, want := range map[string]string{
		"mcp__srv__tool": "mcp:srv/tool", "mcp:srv/tool": "mcp:srv/tool", "bash": "bash", "ghost": "ghost",
	} {
		if got := ResolveToolName(adv, in); got != want {
			t.Errorf("ResolveToolName(%q) = %q, want %q", in, got, want)
		}
	}
}
