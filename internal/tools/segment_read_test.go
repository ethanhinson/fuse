package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/segment"
)

// writeSeg writes a segment file + index entry into segDir for the test.
func writeTestSegments(t *testing.T, segDir string, segs []segment.Segment) {
	t.Helper()
	if err := os.MkdirAll(segDir, 0o700); err != nil {
		t.Fatal(err)
	}
	idx := segment.Index{SessionID: "test"}
	for i, s := range segs {
		name := segment.FileName(s.TurnStart, s.TurnEnd, i+1)
		if err := os.WriteFile(filepath.Join(segDir, name), []byte(segment.RenderSegment(s)), 0o600); err != nil {
			t.Fatal(err)
		}
		idx.Segments = append(idx.Segments, segment.IndexEntry{
			TurnStart: s.TurnStart, TurnEnd: s.TurnEnd, Tools: s.Tools,
			Path: name, TS: s.TS,
		})
	}
	b, _ := json.Marshal(idx)
	if err := os.WriteFile(filepath.Join(segDir, segment.IndexFileName), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func execSegRead(t *testing.T, segDir, args string) Result {
	t.Helper()
	tool := NewSegmentRead(func() string { return segDir })
	return tool.Execute(context.Background(), args)
}

func TestSegmentReadSingleTurn(t *testing.T) {
	dir := t.TempDir()
	segDir := filepath.Join(dir, "segments")
	ts := time.Now().UTC()
	writeTestSegments(t, segDir, []segment.Segment{
		{TurnStart: 10, TurnEnd: 14, Tools: []string{"read_file"}, TS: ts,
			Summary: "s1", Messages: msgs("read_file", "alpha content")},
		{TurnStart: 20, TurnEnd: 24, Tools: []string{"grep"}, TS: ts,
			Summary: "s2", Messages: msgs("grep", "beta content")},
	})

	// Single turn "12" overlaps only the 10-14 segment.
	res := execSegRead(t, segDir, `{"turn_range":"12"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if !strings.Contains(res.Output, "alpha content") {
		t.Errorf("expected alpha content, got:\n%s", res.Output)
	}
	if strings.Contains(res.Output, "beta content") {
		t.Errorf("turn 12 must not include the 20-24 segment")
	}
}

func TestSegmentReadRangeOverlap(t *testing.T) {
	dir := t.TempDir()
	segDir := filepath.Join(dir, "segments")
	ts := time.Now().UTC()
	writeTestSegments(t, segDir, []segment.Segment{
		{TurnStart: 10, TurnEnd: 14, Tools: []string{"read_file"}, TS: ts,
			Summary: "s1", Messages: msgs("read_file", "alpha content")},
		{TurnStart: 16, TurnEnd: 22, Tools: []string{"grep"}, TS: ts,
			Summary: "s2", Messages: msgs("grep", "beta content")},
	})

	// Range "12-18" overlaps both segments (10-14 and 16-22).
	res := execSegRead(t, segDir, `{"turn_range":"12-18"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if !strings.Contains(res.Output, "alpha content") || !strings.Contains(res.Output, "beta content") {
		t.Errorf("range 12-18 must include both overlapping segments; got:\n%s", res.Output)
	}
}

func TestSegmentReadToolFilter(t *testing.T) {
	dir := t.TempDir()
	segDir := filepath.Join(dir, "segments")
	ts := time.Now().UTC()
	writeTestSegments(t, segDir, []segment.Segment{
		{TurnStart: 10, TurnEnd: 20, Tools: []string{"read_file", "grep"}, TS: ts,
			Summary: "s1", Messages: []model.Message{
				{Role: "tool", Name: "read_file", Content: "file body here"},
				{Role: "tool", Name: "grep", Content: "grep body here"},
			}},
	})

	res := execSegRead(t, segDir, `{"turn_range":"10-20","tool_filter":"read_file"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if !strings.Contains(res.Output, "file body here") {
		t.Errorf("tool_filter=read_file must keep read_file result; got:\n%s", res.Output)
	}
	if strings.Contains(res.Output, "grep body here") {
		t.Errorf("tool_filter=read_file must drop grep result; got:\n%s", res.Output)
	}
}

func TestSegmentReadEmptyRangeCleanResult(t *testing.T) {
	dir := t.TempDir()
	segDir := filepath.Join(dir, "segments")
	ts := time.Now().UTC()
	writeTestSegments(t, segDir, []segment.Segment{
		{TurnStart: 10, TurnEnd: 14, TS: ts, Summary: "s1", Messages: msgs("read_file", "x")},
	})
	// Range 100-200 matches nothing: a clean "no segments" result, not an error.
	res := execSegRead(t, segDir, `{"turn_range":"100-200"}`)
	if res.IsError {
		t.Errorf("empty range must NOT be an error: %s", res.Output)
	}
	if !strings.Contains(strings.ToLower(res.Output), "no segments") {
		t.Errorf("empty range should say 'no segments'; got: %q", res.Output)
	}
}

func TestSegmentReadOversizedBounded(t *testing.T) {
	dir := t.TempDir()
	segDir := filepath.Join(dir, "segments")
	ts := time.Now().UTC()
	// Many big segments so the selection exceeds the cap.
	var segs []segment.Segment
	big := strings.Repeat("Q", 20000)
	for i := 0; i < 30; i++ {
		segs = append(segs, segment.Segment{
			TurnStart: i, TurnEnd: i, Tools: []string{"read_file"}, TS: ts,
			Summary: "s", Messages: msgs("read_file", big),
		})
	}
	writeTestSegments(t, segDir, segs)

	res := execSegRead(t, segDir, `{"turn_range":"0-29"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if !strings.Contains(res.Output, "more segments") && !strings.Contains(res.Output, "narrow turn_range") {
		t.Errorf("oversized selection must carry a bounded trailer; got tail:\n%s", tail(res.Output, 300))
	}
}

func msgs(tool, content string) []model.Message {
	return []model.Message{{Role: "tool", Name: tool, Content: content}}
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
