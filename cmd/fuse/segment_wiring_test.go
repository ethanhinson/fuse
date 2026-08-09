package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/segment/fssink"
	"github.com/ethanhinson/fuse/internal/session"
)

// TestActiveSegmentSinkDefaultsNil: with no sink installed, installSummarizer
// wires the agent with a nil sink (→ the no-op default inside the agent).
func TestActiveSegmentSinkResolvesInstalled(t *testing.T) {
	prev := activeSegmentSink
	t.Cleanup(func() { activeSegmentSink = prev })

	activeSegmentSink = nil
	if got := currentSegmentSink(); got != nil {
		t.Errorf("currentSegmentSink() = %v, want nil when none installed", got)
	}

	base := t.TempDir()
	sink := fssink.NewFSSegmentSink(base, "root-xyz")
	setActiveSegmentSink(sink)
	got := currentSegmentSink()
	if got == nil {
		t.Fatalf("currentSegmentSink() = nil after setActiveSegmentSink")
	}
	if _, ok := got.(*fssink.FSSegmentSink); !ok {
		t.Errorf("currentSegmentSink() type = %T, want *fssink.FSSegmentSink", got)
	}
	var _ agent.SegmentSink = got // compile-time interface satisfaction
}

// TestNewSessionLoggerPerSessionDir: a new session's log lands under
// <base>/<session-id>/session.jsonl.
func TestNewSessionLoggerPerSessionDir(t *testing.T) {
	base := t.TempDir()
	lg, err := session.NewSessionLogger(base, "root-abc")
	if err != nil {
		t.Fatalf("NewSessionLogger err = %v", err)
	}
	defer lg.Close()

	want := filepath.Join(base, "root-abc", "session.jsonl")
	if _, err := os.Stat(want); err != nil {
		t.Errorf("session.jsonl not under per-session dir: %v", err)
	}
}

// TestFlatLogsStillLoad: an existing flat *.jsonl log in the base dir is left
// alone and remains readable (no migration).
func TestFlatLogsStillLoad(t *testing.T) {
	base := t.TempDir()
	flat := filepath.Join(base, "2026-01-01-abcdef.jsonl")
	if err := os.WriteFile(flat, []byte(`{"kind":"spawn"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Opening a new per-session logger must not disturb the flat log.
	lg, err := session.NewSessionLogger(base, "root-new")
	if err != nil {
		t.Fatalf("NewSessionLogger err = %v", err)
	}
	defer lg.Close()
	if _, err := os.Stat(flat); err != nil {
		t.Errorf("flat log disturbed: %v", err)
	}
}
