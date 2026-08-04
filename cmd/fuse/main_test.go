package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestModelsSubcommandLists(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var out, errb bytes.Buffer
	code := run([]string{"models"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, errb.String())
	}
	if !strings.Contains(out.String(), "deepseek-flash") || !strings.Contains(out.String(), "claude") {
		t.Errorf("models output = %q", out.String())
	}
}

func TestNoTaskIsUsageError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var out, errb bytes.Buffer
	code := run([]string{}, &out, &errb)
	if code == 0 {
		t.Fatal("expected non-zero exit with no task")
	}
	if !strings.Contains(errb.String(), "usage") && !strings.Contains(errb.String(), "task") {
		t.Errorf("stderr = %q", errb.String())
	}
}

func TestUnknownModelIsError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var out, errb bytes.Buffer
	code := run([]string{"--model", "nope", "do a thing"}, &out, &errb)
	if code == 0 {
		t.Fatal("expected error for unknown model")
	}
	if !strings.Contains(errb.String(), "nope") {
		t.Errorf("stderr = %q", errb.String())
	}
}
