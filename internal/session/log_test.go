package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/archive"
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

// TestSweepOldArchivesStale: SweepOld is now NON-DESTRUCTIVE — a stale log is
// gzip-compressed to "<name>.gz" with a metadata sidecar, not deleted, and its
// content is still recoverable byte-for-byte (change 0030 scope expansion).
func TestSweepOldArchivesStale(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "2000-01-01-aaaaaa.jsonl")
	fresh := filepath.Join(dir, "2999-01-01-bbbbbb.jsonl")
	oldBody := `{"ts":"2000-01-01T00:00:00Z","node_id":"n1","label":"root","kind":"spawn"}
{"ts":"2000-01-01T00:00:01Z","node_id":"n2","parent_id":"n1","kind":"done","depth":1}
`
	if err := os.WriteFile(old, []byte(oldBody), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fresh, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Backdate the "old" file well past the cutoff.
	past := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}

	SweepOld(dir, 7*24*time.Hour, "*.jsonl")

	// Original gone, .gz + sidecar present.
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("stale log original should have been archived (removed) after gzip")
	}
	if _, err := os.Stat(old + ".gz"); err != nil {
		t.Errorf("stale log not gzip-archived: %v", err)
	}
	if _, err := os.Stat(old + ".gz.meta.yml"); err != nil {
		t.Errorf("stale log metadata sidecar not written: %v", err)
	}
	// Fresh log untouched.
	if _, err := os.Stat(fresh); err != nil {
		t.Error("fresh log should survive")
	}
	// The archived content is recoverable byte-for-byte.
	got, err := archive.Open(old)
	if err != nil {
		t.Fatalf("recover archived log: %v", err)
	}
	if string(got) != oldBody {
		t.Errorf("archived log not byte-identical:\n got %q\nwant %q", got, oldBody)
	}
	// The sidecar carries session-domain fields describing WHAT is in the file.
	sc, err := os.ReadFile(old + ".gz.meta.yml")
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"entry_count", "node_ids", "root_label", "kinds"} {
		if !strings.Contains(string(sc), field) {
			t.Errorf("sidecar missing session field %q:\n%s", field, sc)
		}
	}
}

// TestSweepOldSkipsAlreadyGz: an already-archived ".gz" log is left alone.
func TestSweepOldSkipsAlreadyGz(t *testing.T) {
	dir := t.TempDir()
	gz := filepath.Join(dir, "2000-01-01-cccccc.jsonl.gz")
	if err := os.WriteFile(gz, []byte("\x1f\x8bfake"), 0o644); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-30 * 24 * time.Hour)
	_ = os.Chtimes(gz, past, past)

	SweepOld(dir, 7*24*time.Hour, "*.jsonl")

	if _, err := os.Stat(gz); err != nil {
		t.Error("already-archived .gz log should be left alone")
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

// TestSweepOldSegmentsCompressesLegacyPlaintextKeepsDiscoverable: the segment
// sweep is now NON-DESTRUCTIVE. New segments are born ".md.gz" (Part A) so the
// sweep skips them; a LEGACY plaintext ".md" older than the horizon is gzipped
// in place to ".md.gz" and the index Path re-pointed — it stays discoverable and
// recoverable, never deleted (change 0030 scope expansion).
func TestSweepOldSegmentsCompressesLegacyPlaintextKeepsDiscoverable(t *testing.T) {
	base := t.TempDir()
	segDir := writeSegForSweep(t, base, "sess-a", []string{"1-2-1.md", "3-4-1.md"})
	// Give the legacy plaintext a real rendered body so recovery can be asserted.
	rendered := "---\nturn_start: 1\nturn_end: 2\n---\n\n## Summary\n\ns\n\n## Raw region\n\n```json\n[]\n```\n"
	if err := os.WriteFile(filepath.Join(segDir, "1-2-1.md"), []byte(rendered), 0o600); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(segDir, "1-2-1.md")
	past := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(stale, past, past); err != nil {
		t.Fatal(err)
	}

	SweepOldSegments(base, 14*24*time.Hour)

	// Legacy plaintext compressed in place, NOT deleted.
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("legacy plaintext .md should be compressed away (replaced by .md.gz)")
	}
	if _, err := os.Stat(stale + ".gz"); err != nil {
		t.Errorf("legacy .md not gzipped in place: %v", err)
	}
	// Recoverable byte-for-byte through the transparent reader.
	got, err := archive.Open(stale)
	if err != nil {
		t.Fatalf("recover compressed segment: %v", err)
	}
	if string(got) != rendered {
		t.Errorf("compressed segment not byte-identical")
	}
	// index.json: the entry stays (still discoverable), Path re-pointed to .md.gz.
	b, err := os.ReadFile(filepath.Join(segDir, "index.json"))
	if err != nil {
		t.Fatalf("index.json read: %v", err)
	}
	if !strings.Contains(string(b), "1-2-1.md.gz") {
		t.Errorf("index.json Path not re-pointed to the compressed name:\n%s", b)
	}
	if !strings.Contains(string(b), "3-4-1.md") {
		t.Error("index.json lost the untouched segment")
	}
}

// TestSweepOldSegmentsSkipsBornCompressed: a segment already stored as ".md.gz"
// is never touched (no double-compress, no delete), even when stale.
func TestSweepOldSegmentsSkipsBornCompressed(t *testing.T) {
	base := t.TempDir()
	segDir := filepath.Join(base, "sess-gz", "segments")
	if err := os.MkdirAll(segDir, 0o700); err != nil {
		t.Fatal(err)
	}
	gzSeg := filepath.Join(segDir, "1-1-1.md.gz")
	if err := os.WriteFile(gzSeg, []byte("\x1f\x8bfake"), 0o600); err != nil {
		t.Fatal(err)
	}
	idx := `{"session_id":"sess-gz","segments":[{"path":"1-1-1.md.gz"}]}`
	if err := os.WriteFile(filepath.Join(segDir, "index.json"), []byte(idx), 0o600); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-90 * 24 * time.Hour)
	_ = os.Chtimes(gzSeg, past, past)

	SweepOldSegments(base, 14*24*time.Hour)

	if _, err := os.Stat(gzSeg); err != nil {
		t.Error("born-compressed .md.gz segment should never be deleted by the sweep")
	}
	b, _ := os.ReadFile(filepath.Join(segDir, "index.json"))
	if !strings.Contains(string(b), "1-1-1.md.gz") {
		t.Error("born-compressed segment wrongly pruned from index")
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

	// The sweep descended the symlink and compressed the legacy plaintext in place
	// (non-destructive): the original .md is gone but the .md.gz replaces it.
	if _, err := os.Stat(filepath.Join(realSeg, "9-9-1.md")); !os.IsNotExist(err) {
		t.Error("stale legacy segment inside a symlinked session dir was not compressed (IsDir skipped the symlink)")
	}
	if _, err := os.Stat(filepath.Join(realSeg, "9-9-1.md.gz")); err != nil {
		t.Errorf("stale legacy segment not gzipped in place inside symlinked dir: %v", err)
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
