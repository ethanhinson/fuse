package segment

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/model"
)

func TestRenderSegmentFormat(t *testing.T) {
	ts := time.Date(2026, 8, 8, 17, 22, 4, 0, time.UTC)
	seg := Segment{
		TurnStart:    12,
		TurnEnd:      18,
		Tools:        []string{"read_file", "grep", "bash"},
		TokensBefore: 12040,
		TokensAfter:  1180,
		TS:           ts,
		Summary:      "Objective: do a thing\nNext: keep going",
		Messages: []model.Message{
			{Role: "assistant", Content: "let me read"},
			{Role: "tool", Name: "read_file", Content: "file contents here"},
		},
	}
	out := RenderSegment(seg)

	// Front-matter fences and exact field names.
	if !strings.HasPrefix(out, "---\n") {
		t.Fatalf("segment must open with a front-matter fence; got:\n%s", out)
	}
	for _, want := range []string{
		"turn_start: 12",
		"turn_end: 18",
		"tools: [read_file, grep, bash]",
		"tokens_before: 12040",
		"tokens_after: 1180",
		"ts: 2026-08-08T17:22:04Z",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("front-matter missing %q in:\n%s", want, out)
		}
	}
	// Sections.
	if !strings.Contains(out, "## Summary") {
		t.Errorf("missing ## Summary section")
	}
	if !strings.Contains(out, "Objective: do a thing") {
		t.Errorf("summary body missing")
	}
	if !strings.Contains(out, "## Raw region") {
		t.Errorf("missing ## Raw region section")
	}
	// Raw region renders role + tool name + content, unsanitized.
	if !strings.Contains(out, "[read_file]") {
		t.Errorf("raw region missing tool name marker")
	}
	if !strings.Contains(out, "file contents here") {
		t.Errorf("raw region missing tool content")
	}
	if !strings.Contains(out, "let me read") {
		t.Errorf("raw region missing assistant content")
	}
}

func TestFileNameFormat(t *testing.T) {
	if got := FileName(12, 18, 1); got != "12-18-1.md" {
		t.Errorf("FileName = %q, want 12-18-1.md", got)
	}
	if got := FileName(0, 0, 3); got != "0-0-3.md" {
		t.Errorf("FileName = %q, want 0-0-3.md", got)
	}
}

func TestIndexJSONSchema(t *testing.T) {
	idx := Index{
		SessionID: "root-abc",
		Segments: []IndexEntry{
			{
				TurnStart:    12,
				TurnEnd:      18,
				Tools:        []string{"read_file", "grep"},
				TokensBefore: 12040,
				TokensAfter:  1180,
				Path:         "12-18-1.md",
				TS:           time.Date(2026, 8, 8, 17, 22, 4, 0, time.UTC),
			},
		},
	}
	b, err := json.Marshal(idx)
	if err != nil {
		t.Fatalf("marshal index: %v", err)
	}
	s := string(b)
	for _, want := range []string{
		`"session_id":"root-abc"`,
		`"turn_start":12`,
		`"turn_end":18`,
		`"tools":["read_file","grep"]`,
		`"tokens_before":12040`,
		`"tokens_after":1180`,
		`"path":"12-18-1.md"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("index JSON missing %q in:\n%s", want, s)
		}
	}

	// Round-trip.
	var back Index
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal index: %v", err)
	}
	if back.SessionID != "root-abc" || len(back.Segments) != 1 {
		t.Fatalf("round-trip lost data: %+v", back)
	}
	if back.Segments[0].Path != "12-18-1.md" {
		t.Errorf("path not relative to segments/: %q", back.Segments[0].Path)
	}
}
