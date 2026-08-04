package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestEditFileReplacesUnique(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.go")
	os.WriteFile(p, []byte("package main\nfunc old() {}\n"), 0o644)
	res := NewEditFile().Execute(context.Background(), `{"path":"`+p+`","old_string":"func old() {}","new_string":"func new() {}"}`)
	if res.IsError {
		t.Fatal(res.Output)
	}
	got, _ := os.ReadFile(p)
	if string(got) != "package main\nfunc new() {}\n" {
		t.Errorf("content = %q", string(got))
	}
}

func TestEditFileMissingStringIsError(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.go")
	os.WriteFile(p, []byte("hello\n"), 0o644)
	res := NewEditFile().Execute(context.Background(), `{"path":"`+p+`","old_string":"absent","new_string":"x"}`)
	if !res.IsError {
		t.Fatal("expected error when old_string not found")
	}
}

func TestEditFileNonUniqueIsError(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.go")
	os.WriteFile(p, []byte("x\nx\n"), 0o644)
	res := NewEditFile().Execute(context.Background(), `{"path":"`+p+`","old_string":"x","new_string":"y"}`)
	if !res.IsError {
		t.Fatal("expected error when old_string is not unique")
	}
}
