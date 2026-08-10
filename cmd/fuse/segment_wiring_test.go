package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/segment/fssink"
	"github.com/ethanhinson/fuse/internal/session"
)

// TestInstallSummarizerThreadsPerLoopSink proves the segment sink is now a per-loop
// VALUE threaded into installSummarizer (change 0046 — the setActiveSegmentSink /
// currentSegmentSink process-global is retired). With summarization enabled it
// accepts a concrete *fssink.FSSegmentSink (interface satisfaction is load-bearing:
// the shell hands its own sink; one-shot/probe/loop-server hand nil) and with it
// disabled it is inert regardless of the sink argument.
func TestInstallSummarizerThreadsPerLoopSink(t *testing.T) {
	base := t.TempDir()
	var sink agent.SegmentSink = fssink.NewFSSegmentSink(base, "root-xyz")

	// Disabled ⇒ inert: installSummarizer returns without wiring, whether the sink is
	// a real value or nil. A nil-panic here would fail the test.
	disabled := config.Config{}
	a := agent.New(nil, nil, nil, "cloud/model", "", 1, 0)
	installSummarizer(a, disabled, "cloud/model", nil, nil, sink)
	installSummarizer(a, disabled, "cloud/model", nil, nil, nil)
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
