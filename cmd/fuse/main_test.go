package main

import (
	"bytes"
	"os"
	"path/filepath"
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

// TestOneShotNoModelFlagUsesConfigDefault pins the flag's own promise —
// "--model ... (default: config default)" — on the one-shot path (change
// 0073). run() used to pass the raw flag straight into runtime.LoopConfig, so
// an unflagged `fuse "<task>"` reached BuildAgent as model "" and died with
// `unknown model ""` even with models.default set; shell mode (runShell seeds
// alias from reg.Default) and research-probe were both already correct.
func TestOneShotNoModelFlagUsesConfigDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".fuse"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfgBody := "models:\n  default: scripted\n  scripted:\n    id: cloud/scripted-mini\n"
	if err := os.WriteFile(filepath.Join(home, ".fuse", "config.yml"), []byte(cfgBody), 0o644); err != nil {
		t.Fatal(err)
	}
	scriptedGateway(t, "DEFAULT-MODEL-REPLY")

	var out, errb bytes.Buffer
	if code := run([]string{"say hi"}, &out, &errb); code != 0 {
		t.Fatalf("run with no --model exit=%d, want 0; stderr=%s", code, errb.String())
	}
	if !strings.Contains(out.String(), "DEFAULT-MODEL-REPLY") {
		t.Errorf("stdout = %q, want the scripted reply (run must have used the config default model)", out.String())
	}
}
