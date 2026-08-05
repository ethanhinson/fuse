package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/version"
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

func TestRun_Help(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var out, errb bytes.Buffer
	code := run([]string{"help"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, errb.String())
	}
	s := out.String()
	for _, want := range []string{`'########:'##`, version.Version, "models", "shell", "mcps", "help"} {
		if !strings.Contains(s, want) {
			t.Errorf("help output missing %q\noutput:\n%s", want, s)
		}
	}
}

func TestRun_NoArgs_ShowsBanner(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var out, errb bytes.Buffer
	code := run(nil, &out, &errb)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), `'########:'##`) {
		t.Errorf("no-args stderr missing banner wordmark\nstderr:\n%s", errb.String())
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
