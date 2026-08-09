package fssink

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/segment"
)

// compile-time: FSSegmentSink satisfies the agent.SegmentSink interface.
var _ agent.SegmentSink = (*FSSegmentSink)(nil)

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func sampleRegion(ts, te int) agent.SegmentRegion {
	return agent.SegmentRegion{
		TurnStart:    ts,
		TurnEnd:      te,
		Messages:     []model.Message{{Role: "tool", Name: "read_file", Content: "raw body"}},
		Summary:      "Objective: x",
		ToolNames:    []string{"read_file"},
		TokensBefore: 1000,
		TokensAfter:  100,
	}
}

func TestFSSinkArchiveWritesFileAndIndex(t *testing.T) {
	base := t.TempDir()
	sink := NewFSSegmentSink(base, "sess-1")

	ptr, err := sink.Archive(sampleRegion(12, 18))
	if err != nil {
		t.Fatalf("Archive err = %v", err)
	}

	segDir := filepath.Join(base, "sess-1", "segments")
	wantFile := filepath.Join(segDir, "12-18-1.md.gz")
	if ptr != wantFile {
		t.Errorf("pointer = %q, want absolute path %q", ptr, wantFile)
	}
	// The on-disk segment is gzip-compressed (born compressed, change 0030 scope
	// expansion): assert the magic bytes AND that LoadSegment transparently
	// recovers the raw body.
	raw, err := os.ReadFile(wantFile)
	if err != nil {
		t.Fatalf("segment file not written: %v", err)
	}
	if len(raw) < 2 || raw[0] != 0x1f || raw[1] != 0x8b {
		t.Errorf("segment not gzip on disk (first bytes % x)", raw[:minInt(2, len(raw))])
	}
	seg, err := segment.LoadSegment(wantFile)
	if err != nil {
		t.Fatalf("LoadSegment: %v", err)
	}
	if len(seg.Messages) == 0 || !strings.Contains(seg.Messages[0].Content, "raw body") {
		t.Errorf("raw body lost after gzip: %+v", seg.Messages)
	}

	// index.json: exactly one entry.
	idxBytes, err := os.ReadFile(filepath.Join(segDir, "index.json"))
	if err != nil {
		t.Fatalf("index.json not written: %v", err)
	}
	var idx segment.Index
	if err := json.Unmarshal(idxBytes, &idx); err != nil {
		t.Fatalf("index.json bad: %v", err)
	}
	if idx.SessionID != "sess-1" {
		t.Errorf("session_id = %q, want sess-1", idx.SessionID)
	}
	if len(idx.Segments) != 1 {
		t.Fatalf("index entries = %d, want 1", len(idx.Segments))
	}
	e := idx.Segments[0]
	if e.Path != "12-18-1.md.gz" {
		t.Errorf("index path = %q, want 12-18-1.md.gz (on-disk name relative to segments/)", e.Path)
	}
	if e.TurnStart != 12 || e.TurnEnd != 18 {
		t.Errorf("index turn range = %d..%d, want 12..18", e.TurnStart, e.TurnEnd)
	}
}

func TestFSSinkSeqDisambiguatesRepeatedRange(t *testing.T) {
	base := t.TempDir()
	sink := NewFSSegmentSink(base, "sess-1")

	p1, _ := sink.Archive(sampleRegion(1, 3))
	p2, _ := sink.Archive(sampleRegion(1, 3))
	if p1 == p2 {
		t.Fatalf("repeated turn range produced identical paths: %q", p1)
	}
	if !strings.HasSuffix(p1, "1-3-1.md.gz") {
		t.Errorf("first path = %q, want suffix 1-3-1.md.gz", p1)
	}
	if !strings.HasSuffix(p2, "1-3-2.md.gz") {
		t.Errorf("second path = %q, want suffix 1-3-2.md.gz", p2)
	}
	// Both indexed.
	idxBytes, _ := os.ReadFile(filepath.Join(base, "sess-1", "segments", "index.json"))
	var idx segment.Index
	_ = json.Unmarshal(idxBytes, &idx)
	if len(idx.Segments) != 2 {
		t.Errorf("index entries = %d, want 2", len(idx.Segments))
	}
}

func TestFSSinkLazyDirCreation(t *testing.T) {
	base := t.TempDir()
	sink := NewFSSegmentSink(base, "sess-lazy")
	// No dir exists before the first Archive.
	if _, err := os.Stat(filepath.Join(base, "sess-lazy")); !os.IsNotExist(err) {
		t.Fatalf("session dir should not exist before first Archive")
	}
	if _, err := sink.Archive(sampleRegion(1, 1)); err != nil {
		t.Fatalf("Archive err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(base, "sess-lazy", "segments")); err != nil {
		t.Errorf("segments dir not lazily created: %v", err)
	}
}

func TestFSSinkConcurrentArchiveNoCorruption(t *testing.T) {
	base := t.TempDir()
	sink := NewFSSegmentSink(base, "sess-conc")

	const n = 20
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = sink.Archive(sampleRegion(i, i))
		}(i)
	}
	wg.Wait()

	idxBytes, err := os.ReadFile(filepath.Join(base, "sess-conc", "segments", "index.json"))
	if err != nil {
		t.Fatalf("index.json missing: %v", err)
	}
	var idx segment.Index
	if err := json.Unmarshal(idxBytes, &idx); err != nil {
		t.Fatalf("index.json corrupted under concurrency: %v", err)
	}
	if len(idx.Segments) != n {
		t.Errorf("index entries = %d, want %d (lost writes under concurrency)", len(idx.Segments), n)
	}
}
