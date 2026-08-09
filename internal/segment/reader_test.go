package segment

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/model"
)

// TestRawRegionRoundTripLossless is the data-integrity guard for the raw region:
// content that LOOKS like the old human-readable header format (or any other
// header-shaped line) must survive a RenderSegment → LoadSegment round trip
// byte-for-byte. The old line-scanning reader mis-read such lines as message
// boundaries, dropping or mis-attributing content.
func TestRawRegionRoundTripLossless(t *testing.T) {
	orig := []model.Message{
		{Role: "assistant", Content: "let me read the file"},
		{
			Role:       "tool",
			Name:       "read_file",
			ToolCallID: "call_1",
			// A body chock-full of lines that match the OLD rawHeaderRE:
			// "<role> [<name>]:" and "<role>:" plus bare "word:" lines.
			Content: "assistant [read_file]:\n" +
				"tool:\n" +
				"ls -la:\n" +
				"foo: bar\n" + // YAML key
				"a JSON blob: {\"k\": \"v\"}\n" +
				"\n" + // an intentional blank line inside the body
				"final line with no trailing newline",
		},
		{
			Role:    "tool",
			Name:    "grep",
			Content: "grep result\nspanning multiple\nlines",
		},
	}

	seg := Segment{
		TurnStart: 5,
		TurnEnd:   9,
		Tools:     []string{"read_file", "grep"},
		TS:        time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC),
		Summary:   "did some reads",
		Messages:  orig,
	}

	rendered := RenderSegment(seg)

	tmp := filepath.Join(t.TempDir(), "seg.md")
	if err := os.WriteFile(tmp, []byte(rendered), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := LoadSegment(tmp)
	if err != nil {
		t.Fatalf("LoadSegment: %v", err)
	}

	if len(got.Messages) != len(orig) {
		t.Fatalf("message count = %d, want %d\nreconstructed: %+v", len(got.Messages), len(orig), got.Messages)
	}
	for i := range orig {
		w, g := orig[i], got.Messages[i]
		if g.Role != w.Role {
			t.Errorf("msg[%d].Role = %q, want %q", i, g.Role, w.Role)
		}
		if g.Name != w.Name {
			t.Errorf("msg[%d].Name = %q, want %q", i, g.Name, w.Name)
		}
		if g.ToolCallID != w.ToolCallID {
			t.Errorf("msg[%d].ToolCallID = %q, want %q", i, g.ToolCallID, w.ToolCallID)
		}
		if g.Content != w.Content {
			t.Errorf("msg[%d].Content mismatch:\n got: %q\nwant: %q", i, g.Content, w.Content)
		}
	}
}

// TestRawRegionRoundTripToolFilter proves filtering by Name is correct after a
// round trip even when a message body contains header-shaped lines naming a
// different tool.
func TestRawRegionRoundTripToolFilter(t *testing.T) {
	orig := []model.Message{
		{Role: "tool", Name: "read_file", Content: "grep [grep]:\nnot actually grep output"},
		{Role: "tool", Name: "grep", Content: "the real grep output"},
	}
	seg := Segment{TurnStart: 1, TurnEnd: 2, TS: time.Now().UTC(), Summary: "s", Messages: orig}

	tmp := filepath.Join(t.TempDir(), "seg.md")
	if err := os.WriteFile(tmp, []byte(RenderSegment(seg)), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := LoadSegment(tmp)
	if err != nil {
		t.Fatalf("LoadSegment: %v", err)
	}

	var readFile, grep int
	for _, m := range got.Messages {
		switch m.Name {
		case "read_file":
			readFile++
		case "grep":
			grep++
		}
	}
	if readFile != 1 || grep != 1 {
		t.Fatalf("filter-by-Name miscounts after round trip: read_file=%d grep=%d\n%+v", readFile, grep, got.Messages)
	}
}
