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

func TestRendererAssistant(t *testing.T) {
	var buf bytes.Buffer
	r := NewRenderer(&buf, false)
	r.Assistant("hi there")
	if !strings.Contains(buf.String(), "hi there") {
		t.Errorf("output = %q", buf.String())
	}
}

// TestSanitizeDisplayStripsControlBytes: ESC sequences and NULs from tool
// output (e.g. a binary cat'd by bash) must not reach the terminal, and tabs
// must be expanded — the fixed-width compositor counts a tab as one cell
// while the terminal expands it, shearing every column after it.
func TestSanitizeDisplayStripsControlBytes(t *testing.T) {
	in := "ok\x1b[2Jrm\x00ed\rline\nnext\tcol"
	got := sanitizeDisplay(in)
	for _, bad := range []string{"\x1b", "\x00", "\r", "\t"} {
		if strings.Contains(got, bad) {
			t.Errorf("control byte %q survived: %q", bad, got)
		}
	}
	if !strings.Contains(got, "\n") {
		t.Error("newline must survive sanitization")
	}
	if !strings.Contains(got, "next    col") {
		t.Errorf("tab should expand to spaces: %q", got)
	}
	if clean := "plain text\nwith lines"; sanitizeDisplay(clean) != clean {
		t.Error("clean text must pass through unchanged")
	}
}
