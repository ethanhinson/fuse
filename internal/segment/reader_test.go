package segment

import (
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/model"
)

// gzTo writes content gzip-compressed to path (test helper mirroring how the
// sink now stores segments on disk).
func gzTo(t *testing.T, path string, content []byte) {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(content); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// TestLoadSegmentGzipTransparent: a segment stored gzip-compressed at "<n>.md.gz"
// loads to the SAME messages as the identical plaintext ".md" segment. This is
// the segment-seam half of the no-degradation guarantee (change 0030 scope
// expansion).
func TestLoadSegmentGzipTransparent(t *testing.T) {
	orig := []model.Message{
		{Role: "tool", Name: "read_file", Content: "the API key rotation interval is 4217 seconds"},
		{Role: "assistant", Content: "noted"},
	}
	seg := Segment{TurnStart: 3, TurnEnd: 5, TS: time.Now().UTC(), Summary: "s", Messages: orig}
	rendered := []byte(RenderSegment(seg))

	dir := t.TempDir()
	plain := filepath.Join(dir, "3-5-1.md")
	if err := os.WriteFile(plain, rendered, 0o600); err != nil {
		t.Fatal(err)
	}
	gz := filepath.Join(dir, "3-5-2.md.gz")
	gzTo(t, gz, rendered)

	plainSeg, err := LoadSegment(plain)
	if err != nil {
		t.Fatalf("LoadSegment(plain): %v", err)
	}
	gzSeg, err := LoadSegment(gz)
	if err != nil {
		t.Fatalf("LoadSegment(gz): %v", err)
	}
	if len(plainSeg.Messages) != len(gzSeg.Messages) {
		t.Fatalf("message count differs: plain=%d gz=%d", len(plainSeg.Messages), len(gzSeg.Messages))
	}
	for i := range plainSeg.Messages {
		p, g := plainSeg.Messages[i], gzSeg.Messages[i]
		if p.Role != g.Role || p.Name != g.Name || p.ToolCallID != g.ToolCallID || p.Content != g.Content {
			t.Errorf("msg[%d] differs between plain and gz backing:\n plain=%+v\n    gz=%+v", i, p, g)
		}
	}
	if gzSeg.Summary != plainSeg.Summary {
		t.Errorf("summary differs: plain=%q gz=%q", plainSeg.Summary, gzSeg.Summary)
	}
}

// TestLoadSegmentGzFallbackByPath: LoadSegment given a bare ".md" path whose file
// is missing but a "<path>.gz" exists resolves the .gz automatically. Lets an
// index Path pointing at ".md" keep working if the on-disk file became ".md.gz".
func TestLoadSegmentGzFallbackByPath(t *testing.T) {
	orig := []model.Message{{Role: "tool", Name: "grep", Content: "needle 4217"}}
	seg := Segment{TurnStart: 1, TurnEnd: 1, TS: time.Now().UTC(), Summary: "s", Messages: orig}
	rendered := []byte(RenderSegment(seg))

	dir := t.TempDir()
	// Only the .gz exists; the plain .md does not.
	gzTo(t, filepath.Join(dir, "1-1-1.md.gz"), rendered)

	got, err := LoadSegment(filepath.Join(dir, "1-1-1.md"))
	if err != nil {
		t.Fatalf("LoadSegment fallback: %v", err)
	}
	if len(got.Messages) != 1 || got.Messages[0].Content != "needle 4217" {
		t.Errorf("gz fallback did not recover content: %+v", got.Messages)
	}
}

// TestLoadSegmentTruncatedGz: a corrupt/truncated .md.gz errors, never panics.
func TestLoadSegmentTruncatedGz(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "9-9-1.md.gz")
	gzTo(t, path, bytes.Repeat([]byte("x"), 2000))
	full, _ := os.ReadFile(path)
	if err := os.WriteFile(path, full[:len(full)/2], 0o600); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("LoadSegment panicked on truncated gz: %v", r)
		}
	}()
	if _, err := LoadSegment(path); err == nil {
		t.Errorf("LoadSegment on truncated gz should error")
	}
}

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
