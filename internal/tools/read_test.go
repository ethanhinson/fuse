package tools

import (
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// gzWrite writes content gzip-compressed to path (test helper).
func gzWrite(t *testing.T, path string, content []byte) {
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
		t.Fatalf("write: %v", err)
	}
}

func TestReadFileWhole(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	os.WriteFile(p, []byte("l1\nl2\nl3\n"), 0o644)
	res := NewReadFile().Execute(context.Background(), `{"path":"`+p+`"}`)
	if res.IsError {
		t.Fatal(res.Output)
	}
	if !strings.Contains(res.Output, "l1") || !strings.Contains(res.Output, "l3") {
		t.Errorf("output = %q", res.Output)
	}
}

func TestReadFileLineRange(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	os.WriteFile(p, []byte("a\nb\nc\nd\n"), 0o644)
	res := NewReadFile().Execute(context.Background(), `{"path":"`+p+`","start_line":2,"end_line":3}`)
	if res.IsError {
		t.Fatal(res.Output)
	}
	if strings.Contains(res.Output, "a") || !strings.Contains(res.Output, "b") || !strings.Contains(res.Output, "c") || strings.Contains(res.Output, "d") {
		t.Errorf("range output = %q", res.Output)
	}
}

func TestReadFileMissing(t *testing.T) {
	res := NewReadFile().Execute(context.Background(), `{"path":"/no/such/file"}`)
	if !res.IsError {
		t.Fatal("expected error for missing file")
	}
}

// TestReadFileRefusesBinary: reading a compiled binary must error with
// guidance instead of spewing control bytes into context and terminal.
func TestReadFileRefusesBinary(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "prog")
	// Mach-O-ish content: magic + NULs.
	data := append([]byte("\xcf\xfa\xed\xfe__PAGEZERO"), make([]byte, 512)...)
	if err := os.WriteFile(p, data, 0o755); err != nil {
		t.Fatal(err)
	}
	res := NewReadFile().Execute(context.Background(), `{"path":"`+p+`"}`)
	if !res.IsError {
		t.Fatal("binary read should error")
	}
	if !strings.Contains(res.Output, "binary file") {
		t.Errorf("error should say binary: %q", res.Output)
	}
}

// TestReadFileDefaultWindow: unranged reads of large files return the first
// window with a continuation footer.
func TestReadFileDefaultWindow(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "big.txt")
	var sb strings.Builder
	for i := 0; i < 1500; i++ {
		sb.WriteString("line\n")
	}
	if err := os.WriteFile(p, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	res := NewReadFile().Execute(context.Background(), `{"path":"`+p+`"}`)
	if res.IsError {
		t.Fatal(res.Output)
	}
	if got := strings.Count(res.Output, "line\n"); got > 1001 {
		t.Errorf("window not applied: %d lines", got)
	}
	if !strings.Contains(res.Output, "Use start_line=1001 to continue") {
		t.Errorf("continuation footer missing: %q", res.Output[len(res.Output)-120:])
	}
}

// TestReadFileInvertedRangeErrors: end_line < start_line must be a tool
// error, not a slice-bounds panic (crashed the TUI live).
func TestReadFileInvertedRangeErrors(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	var sb strings.Builder
	for i := 0; i < 120; i++ {
		sb.WriteString("line\n")
	}
	os.WriteFile(p, []byte(sb.String()), 0o644)
	res := NewReadFile().Execute(context.Background(), `{"path":"`+p+`","start_line":110,"end_line":60}`)
	if !res.IsError {
		t.Fatal("inverted range should error")
	}
	if !strings.Contains(res.Output, "invalid range") {
		t.Errorf("error should explain the range: %q", res.Output)
	}
}

// TestReadFileTransparentGunzip: read_file transparently decompresses a .gz
// whose decompressed content is text, returning the SAME text as the plaintext
// original. This keeps the model's generic read_file working on archived spill
// files (change 0030 scope expansion).
func TestReadFileTransparentGunzip(t *testing.T) {
	dir := t.TempDir()
	original := "the API key rotation interval is 4217 seconds\nsecond line\n"
	plain := filepath.Join(dir, "spill.txt")
	if err := os.WriteFile(plain, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	gz := filepath.Join(dir, "spill2.txt.gz")
	gzWrite(t, gz, []byte(original))

	plainRes := NewReadFile().Execute(context.Background(), `{"path":"`+plain+`"}`)
	gzRes := NewReadFile().Execute(context.Background(), `{"path":"`+gz+`"}`)
	if gzRes.IsError {
		t.Fatalf("read_file on .gz errored: %s", gzRes.Output)
	}
	if gzRes.Output != plainRes.Output {
		t.Errorf("gz read differs from plaintext read:\n gz=%q\nplain=%q", gzRes.Output, plainRes.Output)
	}
	if !strings.Contains(gzRes.Output, "4217") {
		t.Errorf("needle not found in gz read: %q", gzRes.Output)
	}
}

// TestReadFileGzFallbackByBasePath: read_file given a bare path whose file is
// missing but "<path>.gz" exists resolves the .gz automatically — the spill
// recovery hint keeps working after archival even if it names the .txt.
func TestReadFileGzFallbackByBasePath(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "spill.txt")
	gzWrite(t, base+".gz", []byte("needle 4217 here\n"))
	res := NewReadFile().Execute(context.Background(), `{"path":"`+base+`"}`)
	if res.IsError {
		t.Fatalf("read_file .gz fallback errored: %s", res.Output)
	}
	if !strings.Contains(res.Output, "4217") {
		t.Errorf("fallback did not recover content: %q", res.Output)
	}
}

// TestReadFileRefusesGzippedBinary: a .gz whose DECOMPRESSED content is binary
// must still be refused — no regression of the binary guard.
func TestReadFileRefusesGzippedBinary(t *testing.T) {
	dir := t.TempDir()
	gz := filepath.Join(dir, "prog.gz")
	binary := append([]byte("\xcf\xfa\xed\xfe__PAGEZERO"), make([]byte, 512)...)
	gzWrite(t, gz, binary)
	res := NewReadFile().Execute(context.Background(), `{"path":"`+gz+`"}`)
	if !res.IsError {
		t.Fatal("gzipped binary should still be refused")
	}
	if !strings.Contains(res.Output, "binary file") {
		t.Errorf("error should say binary: %q", res.Output)
	}
}

// TestReadFileTruncatedGzGraceful: a truncated .gz errors gracefully (no panic).
func TestReadFileTruncatedGzGraceful(t *testing.T) {
	dir := t.TempDir()
	gz := filepath.Join(dir, "trunc.txt.gz")
	gzWrite(t, gz, bytes.Repeat([]byte("data"), 1000))
	full, _ := os.ReadFile(gz)
	if err := os.WriteFile(gz, full[:len(full)/2], 0o600); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("read_file panicked on truncated gz: %v", r)
		}
	}()
	res := NewReadFile().Execute(context.Background(), `{"path":"`+gz+`"}`)
	if !res.IsError {
		t.Errorf("truncated gz should error")
	}
}
