package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestListDirectory(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "b.go"), []byte("x"), 0o644)
	res := NewListDirectory().Execute(context.Background(), `{"path":"`+dir+`"}`)
	if res.IsError {
		t.Fatal(res.Output)
	}
	if !strings.Contains(res.Output, "a.txt") || !strings.Contains(res.Output, "b.go") {
		t.Errorf("output = %q", res.Output)
	}
}

func TestListDirectoryGlob(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "b.go"), []byte("x"), 0o644)
	res := NewListDirectory().Execute(context.Background(), `{"path":"`+dir+`","glob":"*.go"}`)
	if res.IsError {
		t.Fatal(res.Output)
	}
	if strings.Contains(res.Output, "a.txt") || !strings.Contains(res.Output, "b.go") {
		t.Errorf("glob output = %q", res.Output)
	}
}
