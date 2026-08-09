package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoggerWriteAndClose(t *testing.T) {
	dir := t.TempDir()
	l, err := NewLogger(dir)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	if err := l.Write(LogEntry{NodeID: "n1", Kind: "done", TS: time.Now()}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := l.Err(); err != nil {
		t.Fatalf("Err after good write: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The entry must be on disk as one JSONL line.
	files, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if len(files) != 1 {
		t.Fatalf("want 1 log file, got %d", len(files))
	}
	data, _ := os.ReadFile(files[0])
	if !strings.Contains(string(data), `"node_id":"n1"`) {
		t.Errorf("log missing entry: %s", data)
	}
}

// TestLoggerSurfacesWriteErrorAtClose is the regression test for #23: a write
// failure must no longer vanish silently — it is latched and returned from Close
// (and available via Err) even though the per-write return was ignored.
func TestLoggerSurfacesWriteErrorAtClose(t *testing.T) {
	dir := t.TempDir()
	l, err := NewLogger(dir)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	// Force writes to fail by closing the underlying file out from under the
	// buffered writer, then filling the buffer so a flush actually hits disk.
	l.f.Close()
	// Ignore the per-write return, exactly like the hot child call site does.
	for i := 0; i < 100; i++ {
		_ = l.Write(LogEntry{NodeID: "n", Kind: "done", TS: time.Now()})
	}
	if l.Err() == nil {
		t.Fatal("Err() should have latched a write failure")
	}
	if l.Close() == nil {
		t.Fatal("Close() should surface the latched write failure")
	}
}

func TestSweepOldRemovesStale(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "2000-01-01-aaaaaa.jsonl")
	fresh := filepath.Join(dir, "2999-01-01-bbbbbb.jsonl")
	for _, p := range []string{old, fresh} {
		if err := os.WriteFile(p, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Backdate the "old" file well past the cutoff.
	past := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}

	SweepOld(dir, 7*24*time.Hour)

	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("stale log should have been swept")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("fresh log should survive")
	}
}
