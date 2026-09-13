package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/version"
)

// TestVersionSubcommand pins the script-safe one-line version output. It does
// NOT pin the literal version string: internal/version.Version is
// ldflags-injected and a release build stamps a different value (see
// internal/version's own test for why pinning it breaks releases).
func TestVersionSubcommand(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var out, errb bytes.Buffer
	if code := run([]string{"version"}, &out, &errb); code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, errb.String())
	}
	s := out.String()
	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	if want := "fuse " + version.Version; lines[0] != want {
		t.Errorf("first line = %q, want %q", lines[0], want)
	}
	if !strings.Contains(s, runtime.Version()) {
		t.Errorf("output missing Go runtime version %q:\n%s", runtime.Version(), s)
	}
	if plat := fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH); !strings.Contains(s, plat) {
		t.Errorf("output missing platform %q:\n%s", plat, s)
	}
	// backends line is present in both build modes; assert only the prefix
	// common to both (fsstore is always compiled in) so this test passes
	// whether or not -tags pgstore was used.
	if !strings.Contains(s, "backends: fsstore") {
		t.Errorf("output missing backends line:\n%s", s)
	}
	if errb.Len() != 0 {
		t.Errorf("version wrote to stderr: %q", errb.String())
	}
	// Script-safe: exactly one trailing newline, no ANSI escapes.
	if !strings.HasSuffix(s, "\n") || strings.HasSuffix(s, "\n\n") {
		t.Errorf("output must end in exactly one newline, got %q", s)
	}
	if strings.Contains(s, "\x1b") {
		t.Errorf("output contains ANSI escapes: %q", s)
	}
}

// TestVersionFlagAliases pins --version and -version as byte-identical aliases.
func TestVersionFlagAliases(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var base, baseErr bytes.Buffer
	if code := run([]string{"version"}, &base, &baseErr); code != 0 {
		t.Fatalf("version exit = %d", code)
	}
	for _, alias := range []string{"--version", "-version"} {
		var out, errb bytes.Buffer
		if code := run([]string{alias}, &out, &errb); code != 0 {
			t.Fatalf("%s exit = %d, stderr=%s", alias, code, errb.String())
		}
		if out.String() != base.String() {
			t.Errorf("%s output differs from `version`:\n got %q\nwant %q", alias, out.String(), base.String())
		}
	}
}

// TestVersionPrecedesConfigLoad is the regression that matters: the user about
// to file a bug report (and a fresh install) has a broken ~/.fuse/config.yml.
// `fuse version` must still answer, which it can only do if the handler returns
// before config.Load(). A broken config is the observable proxy for that
// ordering property.
func TestVersionPrecedesConfigLoad(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".fuse"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Unparseable YAML: config.Load() cannot succeed on this.
	broken := []byte("models: [unterminated\n\t\tbad: :\n")
	if err := os.WriteFile(filepath.Join(home, ".fuse", "config.yml"), broken, 0o600); err != nil {
		t.Fatal(err)
	}

	// Sanity: the same broken config really does fail another subcommand, so
	// this test would catch a version handler placed below config.Load().
	var mOut, mErr bytes.Buffer
	if code := run([]string{"models"}, &mOut, &mErr); code == 0 {
		t.Fatalf("fixture is not broken: `models` succeeded with a malformed config")
	}

	for _, arg := range []string{"version", "--version", "-version"} {
		var out, errb bytes.Buffer
		if code := run([]string{arg}, &out, &errb); code != 0 {
			t.Fatalf("%s with a broken config: exit = %d, stderr=%s", arg, code, errb.String())
		}
		if !strings.HasPrefix(out.String(), "fuse "+version.Version) {
			t.Errorf("%s output = %q", arg, out.String())
		}
	}
}
