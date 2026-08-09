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

	SweepOld(dir, 7*24*time.Hour, "*.jsonl")

	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("stale log should have been swept")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("fresh log should survive")
	}
}

// writeSegForSweep creates <base>/<id>/segments/ with the named .md files and an
// index.json listing them, returning the segments dir.
func writeSegForSweep(t *testing.T, base, id string, names []string) string {
	t.Helper()
	segDir := filepath.Join(base, id, "segments")
	if err := os.MkdirAll(segDir, 0o700); err != nil {
		t.Fatal(err)
	}
	var entries []string
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(segDir, n), []byte("---\n---\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, `{"path":"`+n+`"}`)
	}
	idx := `{"session_id":"` + id + `","segments":[` + join(entries) + `]}`
	if err := os.WriteFile(filepath.Join(segDir, "index.json"), []byte(idx), 0o600); err != nil {
		t.Fatal(err)
	}
	return segDir
}

func join(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += ","
		}
		out += s
	}
	return out
}

func TestSweepOldSegmentsRemovesStalePrunesIndex(t *testing.T) {
	base := t.TempDir()
	segDir := writeSegForSweep(t, base, "sess-a", []string{"1-2-1.md", "3-4-1.md"})
	stale := filepath.Join(segDir, "1-2-1.md")
	past := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(stale, past, past); err != nil {
		t.Fatal(err)
	}

	SweepOldSegments(base, 14*24*time.Hour)

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error(">14d segment should have been swept")
	}
	if _, err := os.Stat(filepath.Join(segDir, "3-4-1.md")); err != nil {
		t.Error("<=14d segment should survive")
	}
	// index.json pruned to drop the swept entry.
	b, err := os.ReadFile(filepath.Join(segDir, "index.json"))
	if err != nil {
		t.Fatalf("index.json read: %v", err)
	}
	if strings.Contains(string(b), "1-2-1.md") {
		t.Error("index.json still references the swept segment")
	}
	if !strings.Contains(string(b), "3-4-1.md") {
		t.Error("index.json lost the surviving segment")
	}
}

func TestSweepOldSegmentsRemovesEmptiedSessionDir(t *testing.T) {
	base := t.TempDir()
	segDir := writeSegForSweep(t, base, "sess-empty", []string{"1-1-1.md"})
	past := time.Now().Add(-30 * 24 * time.Hour)
	_ = os.Chtimes(filepath.Join(segDir, "1-1-1.md"), past, past)

	SweepOldSegments(base, 14*24*time.Hour)

	// The whole session dir should be gone once its last segment is swept.
	if _, err := os.Stat(filepath.Join(base, "sess-empty")); !os.IsNotExist(err) {
		t.Error("emptied session dir should be removed")
	}
}

func TestSweepOldSegmentsDescendsSymlinkedSessionDir(t *testing.T) {
	base := t.TempDir()
	// Real session dir elsewhere, symlinked into base (learning
	// dirent-isdir-skips-symlinks: DirEntry.IsDir() is false for a symlink, so the
	// sweep must os.Stat-fall-back to descend it).
	realHome := t.TempDir()
	realSeg := writeSegForSweep(t, realHome, "real", []string{"9-9-1.md"})
	past := time.Now().Add(-30 * 24 * time.Hour)
	_ = os.Chtimes(filepath.Join(realSeg, "9-9-1.md"), past, past)

	link := filepath.Join(base, "linked-session")
	if err := os.Symlink(filepath.Join(realHome, "real"), link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	SweepOldSegments(base, 14*24*time.Hour)

	if _, err := os.Stat(filepath.Join(realSeg, "9-9-1.md")); !os.IsNotExist(err) {
		t.Error("stale segment inside a symlinked session dir was not swept (IsDir skipped the symlink)")
	}
}

// TestSweepOldPatternRespected: a non-matching pattern leaves files alone.
func TestSweepOldPatternRespected(t *testing.T) {
	dir := t.TempDir()
	md := filepath.Join(dir, "old.md")
	if err := os.WriteFile(md, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-30 * 24 * time.Hour)
	_ = os.Chtimes(md, past, past)

	// Sweeping *.jsonl must not touch a stale *.md.
	SweepOld(dir, 7*24*time.Hour, "*.jsonl")
	if _, err := os.Stat(md); err != nil {
		t.Error("*.md file swept by a *.jsonl pattern sweep")
	}
}
