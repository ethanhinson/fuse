package agent

import (
	"strings"
	"testing"
)

func TestHandleRegistry_AutoAndCollision(t *testing.T) {
	r := NewHandleRegistry()
	h1 := r.Register("node-1", "Coder")
	h2 := r.Register("node-2", "coder") // same base → disambiguated
	if h1 != "@coder" {
		t.Errorf("first handle = %q, want @coder", h1)
	}
	if h2 == h1 {
		t.Errorf("collision not disambiguated: both %q", h1)
	}
	if !strings.HasPrefix(h2, "@coder-") {
		t.Errorf("second handle = %q, want @coder-N", h2)
	}
	if id, ok := r.Resolve("@coder"); !ok || id != "node-1" {
		t.Errorf("resolve @coder = %q,%v", id, ok)
	}
	// Resolve without the @ prefix works too.
	if id, ok := r.Resolve("coder"); !ok || id != "node-1" {
		t.Errorf("resolve bare coder failed: %q,%v", id, ok)
	}
}

func TestHandleRegistry_Rename(t *testing.T) {
	r := NewHandleRegistry()
	r.Register("node-1", "coder")
	if !r.Rename("@coder", "@scout") {
		t.Fatal("rename failed")
	}
	if _, ok := r.Resolve("@coder"); ok {
		t.Error("old handle should be freed")
	}
	if id, ok := r.Resolve("@scout"); !ok || id != "node-1" {
		t.Errorf("new handle should resolve to node-1, got %q,%v", id, ok)
	}
	// HandleFor reflects the rename (fresh render lookup).
	if h, _ := r.HandleFor("node-1"); h != "@scout" {
		t.Errorf("HandleFor = %q, want @scout", h)
	}
	// Renaming onto a taken handle fails.
	r.Register("node-2", "builder")
	if r.Rename("@scout", "@builder") {
		t.Error("rename onto taken handle should fail")
	}
}

func TestSanitizeHandle(t *testing.T) {
	cases := map[string]string{
		"Read A":        "read-a",
		"coder":         "coder",
		"  Web  Search": "web-search",
		"!!!":           "agent",
		"":              "agent",
		"pipeline_run":  "pipeline-run",
	}
	for in, want := range cases {
		if got := sanitizeHandle(in); got != want {
			t.Errorf("sanitizeHandle(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseAside_IntentAndTarget(t *testing.T) {
	cases := []struct {
		text   string
		kind   AsideKind
		target string
	}{
		{"is @coder still running?", AsideStatus, "@coder"},
		{"what is @coder doing", AsideLastTool, "@coder"},
		{"what did @researcher write", AsideWrites, "@researcher"},
		{"how many running", AsideCount, ""},
		{"show the tree", AsideTree, ""},
		{"sing me a song", AsideUnknown, ""},
	}
	for _, c := range cases {
		q := ParseAside(c.text)
		if q.Kind != c.kind {
			t.Errorf("ParseAside(%q).Kind = %v, want %v", c.text, q.Kind, c.kind)
		}
		if q.Target != c.target {
			t.Errorf("ParseAside(%q).Target = %q, want %q", c.text, q.Target, c.target)
		}
	}
}

func TestAnswerAside_FromLiveState(t *testing.T) {
	tree := NewAgentTree("root", "test")
	reg := NewHandleRegistry()
	child := &AgentNode{ID: "c1", ParentID: tree.RootID(), Label: "coder", Status: StatusRunning}
	child.AddEvent(AgentEvent{Kind: KindToolCall, Name: "edit"})
	tree.addNode(child)
	reg.Register("c1", "coder")
	bb := NewBlackboard(tree)
	bb.Put("plan/api", "stuff", "c1", "coder")

	// Status.
	got := AnswerAside(ParseAside("is @coder running"), tree, bb, reg)
	if !strings.Contains(got, "@coder") || !strings.Contains(got, "running") {
		t.Errorf("status answer wrong: %q", got)
	}
	// Last tool.
	got = AnswerAside(ParseAside("what is @coder doing"), tree, bb, reg)
	if !strings.Contains(got, "edit") {
		t.Errorf("last-tool answer wrong: %q", got)
	}
	// Writes.
	got = AnswerAside(ParseAside("what did @coder write"), tree, bb, reg)
	if !strings.Contains(got, "plan/api") {
		t.Errorf("writes answer wrong: %q", got)
	}
	// Count.
	got = AnswerAside(ParseAside("how many running"), tree, bb, reg)
	if !strings.Contains(got, "running") {
		t.Errorf("count answer wrong: %q", got)
	}
	// Unknown → helpful fallback listing capabilities.
	got = AnswerAside(ParseAside("hello"), tree, bb, reg)
	if !strings.Contains(got, "status") || !strings.Contains(got, "tree") {
		t.Errorf("unknown fallback should list capabilities: %q", got)
	}
	// Unknown handle → names live handles.
	got = AnswerAside(ParseAside("is @ghost running"), tree, bb, reg)
	if !strings.Contains(got, "no node @ghost") {
		t.Errorf("unknown-handle answer wrong: %q", got)
	}
}
