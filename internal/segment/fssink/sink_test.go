package fssink

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/artifact"
	artfs "github.com/ethanhinson/fuse/internal/artifact/fsstore"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/segment"
)

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

// source reads back what New(dir, loop) archived, through a fresh store over
// the same directory (as another process would).
func source(dir, loop string) segment.Source {
	return segment.NewArtifactSource(artifact.Binding{
		Store: artfs.New(dir), Tenant: event.DefaultTenant, Loop: event.LoopID(loop),
	})
}

func TestNewArchivesUnderDefaultTenant(t *testing.T) {
	dir := t.TempDir()
	sink := New(dir, "sess-1")
	if sink == nil {
		t.Fatal("New returned a nil sink")
	}

	ptr, err := sink.Archive(sampleRegion(12, 18))
	if err != nil {
		t.Fatalf("Archive err = %v", err)
	}
	const prefix = "art://_default/sess-1/segment/"
	if !strings.HasPrefix(ptr, prefix) {
		t.Fatalf("pointer = %q, want prefix %q", ptr, prefix)
	}
	id := strings.TrimPrefix(ptr, prefix)

	// The body is under <dir>/_default/sess-1/segment/ and gzip-compressed at rest.
	raw, err := os.ReadFile(filepath.Join(dir, "_default", "sess-1", "segment", id))
	if err != nil {
		t.Fatalf("segment body not written: %v", err)
	}
	if len(raw) < 2 || raw[0] != 0x1f || raw[1] != 0x8b {
		t.Errorf("segment not gzip on disk (first bytes % x)", raw[:min(2, len(raw))])
	}

	src := source(dir, "sess-1")
	idx, err := src.ReadIndex(context.Background())
	if err != nil {
		t.Fatalf("ReadIndex: %v", err)
	}
	if len(idx.Segments) != 1 {
		t.Fatalf("index entries = %d, want 1", len(idx.Segments))
	}
	e := idx.Segments[0]
	if e.Path != id || e.TurnStart != 12 || e.TurnEnd != 18 {
		t.Errorf("index entry = %+v, want path %q turns 12..18", e, id)
	}
	seg, err := src.Load(context.Background(), e)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(seg.Messages) == 0 || !strings.Contains(seg.Messages[0].Content, "raw body") {
		t.Errorf("raw body lost: %+v", seg.Messages)
	}
}

func TestRepeatedRangeGetsDistinctArtifacts(t *testing.T) {
	dir := t.TempDir()
	sink := New(dir, "sess-1")

	p1, err1 := sink.Archive(sampleRegion(1, 3))
	p2, err2 := sink.Archive(sampleRegion(1, 3))
	if err1 != nil || err2 != nil {
		t.Fatalf("Archive errs = %v, %v", err1, err2)
	}
	if p1 == p2 {
		t.Fatalf("repeated turn range produced identical pointers: %q", p1)
	}
	idx, _ := source(dir, "sess-1").ReadIndex(context.Background())
	if len(idx.Segments) != 2 {
		t.Errorf("index entries = %d, want 2", len(idx.Segments))
	}
}

func TestLazyDirCreation(t *testing.T) {
	dir := t.TempDir()
	sink := New(dir, "sess-lazy")
	if _, err := os.Stat(filepath.Join(dir, "_default", "sess-lazy")); !os.IsNotExist(err) {
		t.Fatalf("loop dir should not exist before first Archive")
	}
	if _, err := sink.Archive(sampleRegion(1, 1)); err != nil {
		t.Fatalf("Archive err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "_default", "sess-lazy", "segment")); err != nil {
		t.Errorf("segment dir not lazily created: %v", err)
	}
}

func TestConcurrentArchiveNoLostEntries(t *testing.T) {
	dir := t.TempDir()
	sink := New(dir, "sess-conc")

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

	idx, err := source(dir, "sess-conc").ReadIndex(context.Background())
	if err != nil {
		t.Fatalf("index unreadable under concurrency: %v", err)
	}
	if len(idx.Segments) != n {
		t.Errorf("index entries = %d, want %d (lost writes under concurrency)", len(idx.Segments), n)
	}
}

// TestOpenReturnsSinkAndItsBinding: Open hands the shell the sink plus the
// artifact binding it archives into, so the shell can bind its tool registry
// (segment_read, spill) and the TUI to the very binding the sink writes, with
// one code path for both.
func TestOpenReturnsSinkAndItsBinding(t *testing.T) {
	dir := t.TempDir()
	sink, b := Open(dir, "sess-open")
	if sink == nil {
		t.Fatal("Open returned a nil sink")
	}
	if b.Store == nil || b.Tenant != event.DefaultTenant || b.Loop != "sess-open" {
		t.Fatalf("binding = %+v, want store under default tenant, loop sess-open", b)
	}
	ptr, err := sink.Archive(sampleRegion(4, 6))
	if err != nil {
		t.Fatalf("Archive err = %v", err)
	}
	if !strings.HasPrefix(ptr, "art://_default/sess-open/segment/") {
		t.Fatalf("pointer = %q", ptr)
	}
	// The returned binding reads back what the sink archived.
	idx, err := segment.NewArtifactSource(b).ReadIndex(context.Background())
	if err != nil || len(idx.Segments) != 1 || idx.Segments[0].TurnStart != 4 {
		t.Fatalf("binding index = %+v, err %v; want one entry turns 4..6", idx, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "_default", "sess-open", "segment")); err != nil {
		t.Errorf("segment not under artifactsDir: %v", err)
	}
}
