package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
