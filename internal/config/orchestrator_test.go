package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOrchestratorModelIsTrustedOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(path, []byte("orchestrator:\n  model: glm-5.2\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	trusted := Default()
	if err := mergeFile(&trusted, path, true, nil); err != nil {
		t.Fatal(err)
	}
	if trusted.Orchestrator.Model != "glm-5.2" {
		t.Fatalf("trusted file: model = %q", trusted.Orchestrator.Model)
	}

	var warn bytes.Buffer
	old := warnw
	warnw = &warn
	defer func() { warnw = old }()
	untrusted := Default()
	if err := mergeFile(&untrusted, path, false, nil); err != nil {
		t.Fatal(err)
	}
	if untrusted.Orchestrator.Model != "" {
		t.Fatalf("a repo-plantable file set the orchestrator model: %q", untrusted.Orchestrator.Model)
	}
	if !strings.Contains(warn.String(), "orchestrator.model") {
		t.Fatalf("ignored key was not named in the warning: %q", warn.String())
	}
}
