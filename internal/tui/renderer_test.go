package tui

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/tools"
)

func TestRendererToolCallTruncatesUnlessVerbose(t *testing.T) {
	var buf bytes.Buffer
	r := NewRenderer(&buf, false)
	long := strings.Repeat("x", 500)
	r.ToolCall("bash", `{"command":"`+long+`"}`)
	out := buf.String()
	if !strings.Contains(out, "bash") {
		t.Errorf("missing tool name: %q", out)
	}
	if len(out) > 200 {
		t.Errorf("non-verbose call should truncate, got %d chars", len(out))
	}
}

func TestRendererToolResultError(t *testing.T) {
	var buf bytes.Buffer
	r := NewRenderer(&buf, true)
	r.ToolResult("bash", tools.Result{Output: "boom", IsError: true})
	if !strings.Contains(buf.String(), "boom") {
		t.Errorf("error output missing: %q", buf.String())
	}
}

func TestRendererAssistantAndHeader(t *testing.T) {
	var buf bytes.Buffer
	r := NewRenderer(&buf, false)
	r.ModelHeader("deepseek-flash")
	r.Assistant("hi there")
	out := buf.String()
	if !strings.Contains(out, "deepseek-flash") || !strings.Contains(out, "hi there") {
		t.Errorf("output = %q", out)
	}
}
