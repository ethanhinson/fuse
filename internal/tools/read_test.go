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
