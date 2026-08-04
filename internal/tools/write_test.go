package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteFileCreates(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sub", "out.txt")
	res := NewWriteFile().Execute(context.Background(), `{"path":"`+p+`","content":"hello world"}`)
	if res.IsError {
		t.Fatal(res.Output)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello world" {
		t.Errorf("content = %q", string(got))
	}
}

func TestWriteFileRequiresPath(t *testing.T) {
	res := NewWriteFile().Execute(context.Background(), `{"content":"x"}`)
	if !res.IsError {
		t.Fatal("expected error without path")
	}
}
