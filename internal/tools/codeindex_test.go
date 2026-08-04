package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFakeCodeindex creates an executable stub that echoes its args and sets
// CODEINDEX_BIN to point at it.
func writeFakeCodeindex(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "codeindex")
	script := "#!/bin/sh\n" + body + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEINDEX_BIN", bin)
}

func TestCodeindexImpactInvokesBinary(t *testing.T) {
	writeFakeCodeindex(t, `echo "impact: $1 $2"`)
	res := NewCodeindexImpact().Execute(context.Background(), `{"symbol":"Foo"}`)
	if res.IsError {
		t.Fatal(res.Output)
	}
	if !strings.Contains(res.Output, "impact") || !strings.Contains(res.Output, "Foo") {
		t.Errorf("output = %q", res.Output)
	}
}

func TestCodeindexCallersInvokesBinary(t *testing.T) {
	writeFakeCodeindex(t, `echo "callers: $1 $2"`)
	res := NewCodeindexCallers().Execute(context.Background(), `{"symbol":"Bar"}`)
	if res.IsError {
		t.Fatal(res.Output)
	}
	if !strings.Contains(res.Output, "callers") || !strings.Contains(res.Output, "Bar") {
		t.Errorf("output = %q", res.Output)
	}
}

func TestCodeindexRequiresSymbol(t *testing.T) {
	writeFakeCodeindex(t, `echo x`)
	res := NewCodeindexImpact().Execute(context.Background(), `{}`)
	if !res.IsError {
		t.Fatal("expected error without symbol")
	}
}

func TestCodeindexBinaryFailureIsError(t *testing.T) {
	writeFakeCodeindex(t, `echo "boom" >&2; exit 2`)
	res := NewCodeindexImpact().Execute(context.Background(), `{"symbol":"Foo"}`)
	if !res.IsError {
		t.Fatal("expected error on non-zero exit")
	}
	if !strings.Contains(res.Output, "boom") {
		t.Errorf("stderr should surface: %q", res.Output)
	}
}
