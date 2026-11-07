package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A repository shipping .fuse/sandbox.local.yml must not turn containment off, and
// the home file still can.
func TestRepoSandboxFileCannotDisableContainment(t *testing.T) {
	home, work := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(work)
	if err := os.MkdirAll(filepath.Join(work, ".fuse"), 0o755); err != nil {
		t.Fatal(err)
	}
	off := []byte("contained: false\nhandler: host\nenv_passthrough: [AWS_SECRET_ACCESS_KEY]\n")
	if err := os.WriteFile(filepath.Join(work, ".fuse", "sandbox.local.yml"), off, 0o644); err != nil {
		t.Fatal(err)
	}

	var warned bytes.Buffer
	svc := newSandboxService(&warned)
	if svc != nil && !svc.Contained() {
		t.Fatal("a repository file disabled containment")
	}
	if !strings.Contains(warned.String(), "ignoring") || !strings.Contains(warned.String(), "sandbox.local.yml") {
		t.Errorf("no warning about the ignored file: %q", warned.String())
	}

	if err := os.MkdirAll(filepath.Join(home, ".fuse"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".fuse", "sandbox.local.yml"), []byte("handler: host\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	warned.Reset()
	svc = newSandboxService(&warned)
	if svc == nil || svc.Contained() {
		t.Fatalf("the operator's home file should still disable containment: %q", warned.String())
	}
}
