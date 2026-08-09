package archive

import (
	"bytes"
	"compress/gzip"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// writeGz writes content gzip-compressed (Go's gzip.Writer) to path.
func writeGz(t *testing.T, path string, content []byte) {
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
		t.Fatalf("write gz: %v", err)
	}
}

// TestArchiveThenOpenRoundTrip: Archive a plaintext file, then Open the ORIGINAL
// path — content must be byte-identical, the original gone, the .gz + sidecar
// present. (Tier 1, Go gzip.Writer via Archive.)
func TestArchiveThenOpenRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	original := []byte("line one\nline two\n{\"k\":\"v\"}\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}

	metaPath, err := Archive(path, nil)
	if err != nil {
		t.Fatalf("Archive err = %v", err)
	}

	// Original removed; .gz + sidecar present.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("original not removed after Archive")
	}
	if _, err := os.Stat(path + GzSuffix); err != nil {
		t.Errorf(".gz not written: %v", err)
	}
	if _, err := os.Stat(metaPath); err != nil {
		t.Errorf("sidecar not written at %s: %v", metaPath, err)
	}

	// Open(original path) transparently falls back to .gz and gunzips.
	got, err := Open(path)
	if err != nil {
		t.Fatalf("Open err = %v", err)
	}
	if !bytes.Equal(got, original) {
		t.Errorf("round-trip not byte-identical:\n got %q\nwant %q", got, original)
	}
}

// TestOpenHandGzippedGoWriter: hand-gzip content (Go writer) at path.gz and Open
// the base path — byte-identical. Proves Open's magic-byte + .gz fallback.
func TestOpenHandGzippedGoWriter(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "spill.txt")
	original := []byte("the API key rotation interval is 4217 seconds\n")
	writeGz(t, base+GzSuffix, original)

	got, err := Open(base)
	if err != nil {
		t.Fatalf("Open err = %v", err)
	}
	if !bytes.Equal(got, original) {
		t.Errorf("hand-gz round-trip mismatch:\n got %q\nwant %q", got, original)
	}
}

// TestOpenSystemGzipBinary: create the .gz with the SYSTEM gzip binary (not Go's
// gzip.Writer) and prove Open recovers byte-identical content — cross-tool
// compatibility. Skips gracefully if the gzip binary is absent. (Tier 1.)
func TestOpenSystemGzipBinary(t *testing.T) {
	gzipBin, err := exec.LookPath("gzip")
	if err != nil {
		t.Skip("system gzip binary not available")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "sysgz.jsonl")
	original := []byte("needle=4217\nother line\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	// gzip removes the original and writes path.gz.
	cmd := exec.Command(gzipBin, path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("system gzip failed: %v (%s)", err, out)
	}
	if _, err := os.Stat(path + GzSuffix); err != nil {
		t.Fatalf("system gzip did not produce %s.gz: %v", path, err)
	}

	got, err := Open(path)
	if err != nil {
		t.Fatalf("Open err on system-gz file = %v", err)
	}
	if !bytes.Equal(got, original) {
		t.Errorf("system-gzip round-trip mismatch:\n got %q\nwant %q", got, original)
	}
}

// TestOpenPlaintextBackCompat: Open reads an un-gzipped file unchanged (old
// plaintext files stay readable).
func TestOpenPlaintextBackCompat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plain.txt")
	original := []byte("plain content, not gzipped\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Open(path)
	if err != nil {
		t.Fatalf("Open err = %v", err)
	}
	if !bytes.Equal(got, original) {
		t.Errorf("plaintext read mismatch")
	}
}

// TestArchiveSidecarCommonFields: the sidecar carries the common WHAT fields and
// any domain fields the MetaFunc supplies.
func TestArchiveSidecarCommonFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "log.jsonl")
	original := bytes.Repeat([]byte("compressible compressible compressible\n"), 50)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}

	metaPath, err := Archive(path, func(content []byte) map[string]any {
		return map[string]any{"entry_count": 42, "tool_name": "read_file"}
	})
	if err != nil {
		t.Fatalf("Archive err = %v", err)
	}
	b, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(b, &m); err != nil {
		t.Fatalf("sidecar not valid YAML: %v", err)
	}
	for _, k := range []string{"archived_at", "original_name", "original_bytes", "compressed_bytes"} {
		if _, ok := m[k]; !ok {
			t.Errorf("sidecar missing common field %q", k)
		}
	}
	if m["original_name"] != "log.jsonl" {
		t.Errorf("original_name = %v, want log.jsonl", m["original_name"])
	}
	if m["entry_count"] != 42 {
		t.Errorf("domain field entry_count = %v, want 42", m["entry_count"])
	}
	// Compression must actually shrink this repetitive content.
	ob, _ := m["original_bytes"].(int)
	cb, _ := m["compressed_bytes"].(int)
	if cb == 0 || cb >= ob {
		t.Errorf("compression did not shrink: original=%d compressed=%d", ob, cb)
	}
}

// TestArchiveIdempotentAlreadyGz: archiving a path that already ends in .gz is a
// no-op. (Tier 5.)
func TestArchiveIdempotentAlreadyGz(t *testing.T) {
	dir := t.TempDir()
	gzPath := filepath.Join(dir, "already.txt.gz")
	writeGz(t, gzPath, []byte("hi"))
	metaPath, err := Archive(gzPath, nil)
	if err != nil {
		t.Fatalf("Archive on .gz err = %v", err)
	}
	if metaPath != "" {
		t.Errorf("Archive on .gz should be a no-op, got metaPath %q", metaPath)
	}
	if _, err := os.Stat(gzPath); err != nil {
		t.Errorf(".gz was disturbed: %v", err)
	}
}

// TestArchiveIdempotentGzExists: if path.gz already exists, Archive does not
// re-compress or delete; it returns the existing sidecar path. (Tier 5.)
func TestArchiveIdempotentGzExists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "log.jsonl")
	if err := os.WriteFile(path, []byte("fresh"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Pre-existing archived form.
	writeGz(t, path+GzSuffix, []byte("old archived content"))

	metaPath, err := Archive(path, nil)
	if err != nil {
		t.Fatalf("Archive err = %v", err)
	}
	if metaPath != path+GzSuffix+MetaSuffix {
		t.Errorf("metaPath = %q, want existing sidecar path", metaPath)
	}
	// The original must NOT be deleted and the .gz must NOT be overwritten.
	if _, err := os.Stat(path); err != nil {
		t.Errorf("original wrongly removed when .gz already existed: %v", err)
	}
	got, _ := os.ReadFile(path + GzSuffix)
	// It should still decode to the OLD content (untouched).
	dec, err := gzip.NewReader(bytes.NewReader(got))
	if err != nil {
		t.Fatal(err)
	}
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(dec)
	if buf.String() != "old archived content" {
		t.Errorf(".gz was overwritten: got %q", buf.String())
	}
}

// TestOpenTruncatedGzGracefulError: a truncated .gz yields an error, never a
// panic. (Tier 5.)
func TestOpenTruncatedGzGracefulError(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "trunc.txt")
	writeGz(t, base+GzSuffix, bytes.Repeat([]byte("data"), 1000))
	// Truncate the .gz mid-stream (keep the magic bytes so Open tries to gunzip).
	full, _ := os.ReadFile(base + GzSuffix)
	if err := os.WriteFile(base+GzSuffix, full[:len(full)/2], 0o600); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Open panicked on truncated gz: %v", r)
		}
	}()
	if _, err := Open(base); err == nil {
		t.Errorf("Open on truncated gz should error, got nil")
	}
}

// TestOpenEmptyFile: an empty (zero-byte) file opens to empty content, no error.
// (Tier 5.)
func TestOpenEmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.txt")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Open(path)
	if err != nil {
		t.Fatalf("Open empty err = %v", err)
	}
	if len(got) != 0 {
		t.Errorf("empty file opened to %d bytes", len(got))
	}
}

// TestArchiveMissingFile: archiving a non-existent path returns an error.
func TestArchiveMissingFile(t *testing.T) {
	dir := t.TempDir()
	_, err := Archive(filepath.Join(dir, "nope.txt"), nil)
	if err == nil {
		t.Errorf("Archive of missing file should error")
	}
}
