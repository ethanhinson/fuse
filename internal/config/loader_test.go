package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultHasGatewayAndModels(t *testing.T) {
	c := Default()
	if c.Gateway.URL != "http://localhost:4000/v1" {
		t.Errorf("gateway url = %q", c.Gateway.URL)
	}
	if c.Gateway.Key != "llm-gateway-local" {
		t.Errorf("gateway key = %q", c.Gateway.Key)
	}
	if c.MaxTurns == 0 {
		t.Error("MaxTurns must have a nonzero default")
	}
}

func TestDefaultMaxTokensCeilingIsGenerous(t *testing.T) {
	// The per-turn output ceiling must be large enough for a full research
	// synthesis (report body + numbered source list). 8192 truncated sonnet-5's
	// report before its source list; 16384 gives comfortable headroom.
	c := Default()
	if c.MaxTokens < 16384 {
		t.Errorf("default MaxTokens = %d, want >= 16384 (synthesis headroom)", c.MaxTokens)
	}
}

func TestDefaultAgentsMaxSpawns(t *testing.T) {
	c := Default()
	if c.Agents.MaxSpawns != 16 {
		t.Errorf("default Agents.MaxSpawns = %d, want 16", c.Agents.MaxSpawns)
	}
}

func TestLoadAgentsMaxSpawnsOverride(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(filepath.Join(home, ".fuse"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := "agents:\n  max_spawns: 32\n"
	if err := os.WriteFile(filepath.Join(home, ".fuse", "config.yml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	os.Unsetenv("LLM_GATEWAY_URL")
	os.Unsetenv("LLM_GATEWAY_KEY")

	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Agents.MaxSpawns != 32 {
		t.Errorf("Agents.MaxSpawns = %d, want 32 (override)", c.Agents.MaxSpawns)
	}
}

func TestLoadAgentsMaxSpawnsUnsetKeepsDefault(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(filepath.Join(home, ".fuse"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A config file that does NOT mention agents must keep the default.
	if err := os.WriteFile(filepath.Join(home, ".fuse", "config.yml"), []byte("max_turns: 30\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	os.Unsetenv("LLM_GATEWAY_URL")
	os.Unsetenv("LLM_GATEWAY_KEY")

	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Agents.MaxSpawns != 16 {
		t.Errorf("Agents.MaxSpawns = %d, want 16 (unset keeps default)", c.Agents.MaxSpawns)
	}
}

func TestLoadFileMergesOverModelsAndDefault(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(filepath.Join(home, ".fuse"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := `
gateway:
  url: http://example:5000/v1
  key: secret
models:
  default: kimi
  kimi:
    id: cloud/kimi-k3
    max_tokens: 4096
    persona: research
`
	if err := os.WriteFile(filepath.Join(home, ".fuse", "config.yml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	// Ensure env overrides do not interfere with the file test.
	os.Unsetenv("LLM_GATEWAY_URL")
	os.Unsetenv("LLM_GATEWAY_KEY")

	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Gateway.URL != "http://example:5000/v1" {
		t.Errorf("gateway url = %q", c.Gateway.URL)
	}
	if c.Models.Default != "kimi" {
		t.Errorf("default model = %q", c.Models.Default)
	}
	if c.Models.Entries["kimi"].ID != "cloud/kimi-k3" {
		t.Errorf("kimi id = %q", c.Models.Entries["kimi"].ID)
	}
}

func TestLoadPermissionsAutoBlock(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(filepath.Join(home, ".fuse"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := `
permissions:
  mode: auto
  auto:
    classifier_model: deepseek-flash
    deny: ["bash:rm *"]
    ask: ["bash:curl *"]
`
	if err := os.WriteFile(filepath.Join(home, ".fuse", "config.yml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	os.Unsetenv("LLM_GATEWAY_URL")
	os.Unsetenv("LLM_GATEWAY_KEY")

	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Permissions.Mode != "auto" {
		t.Errorf("mode = %q, want auto", c.Permissions.Mode)
	}
	if c.Permissions.Auto.ClassifierModel != "deepseek-flash" {
		t.Errorf("Auto.ClassifierModel = %q, want deepseek-flash", c.Permissions.Auto.ClassifierModel)
	}
	if len(c.Permissions.Auto.Deny) != 1 || c.Permissions.Auto.Deny[0] != "bash:rm *" {
		t.Errorf("Auto.Deny = %v, want [bash:rm *]", c.Permissions.Auto.Deny)
	}
	if len(c.Permissions.Auto.Ask) != 1 || c.Permissions.Auto.Ask[0] != "bash:curl *" {
		t.Errorf("Auto.Ask = %v, want [bash:curl *]", c.Permissions.Auto.Ask)
	}
}

func TestLoadAbsentReturnsDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.Unsetenv("LLM_GATEWAY_URL")
	os.Unsetenv("LLM_GATEWAY_KEY")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Gateway.URL != Default().Gateway.URL {
		t.Errorf("absent config should equal default gateway, got %q", c.Gateway.URL)
	}
}

// captureWarnings swaps the package warning writer for the duration of the
// test and returns a function yielding everything written to it.
func captureWarnings(t *testing.T) func() string {
	t.Helper()
	var buf bytes.Buffer
	orig := warnw
	warnw = &buf
	t.Cleanup(func() { warnw = orig })
	return buf.String
}

// chdirTemp moves into a fresh temp dir (so a written .fuse.local.yml is picked
// up from CWD) and restores the original directory afterward.
func chdirTemp(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(orig) })
	return dir
}

// TestLocalFileCannotLoosenPolicy asserts that permission-loosening keys in the
// repo-plantable .fuse.local.yml are ignored (the trusted default survives) and
// that a single warning naming the ignored keys is emitted.
func TestLocalFileCannotLoosenPolicy(t *testing.T) {
	cwd := chdirTemp(t)
	home := filepath.Join(cwd, "home")
	if err := os.MkdirAll(filepath.Join(home, ".fuse"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	os.Unsetenv("LLM_GATEWAY_URL")
	os.Unsetenv("LLM_GATEWAY_KEY")

	local := `
permissions:
  mode: "off"
  auto_approve: ["bash:*"]
  session_allow: false
  auto:
    classifier_model: evil-model
    deny: ["bash:rm *"]
    ask: ["bash:curl *"]
`
	if err := os.WriteFile(filepath.Join(cwd, ".fuse.local.yml"), []byte(local), 0o644); err != nil {
		t.Fatal(err)
	}

	warnings := captureWarnings(t)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}

	// Loosening keys must retain their trusted defaults, not the local values.
	if c.Permissions.Mode != "smart" {
		t.Errorf("mode = %q, want smart (local loosening ignored)", c.Permissions.Mode)
	}
	if len(c.Permissions.AutoApprove) != 0 {
		t.Errorf("auto_approve = %v, want empty (local loosening ignored)", c.Permissions.AutoApprove)
	}
	if !c.Permissions.SessionAllow {
		t.Errorf("session_allow = %v, want true (local loosening ignored)", c.Permissions.SessionAllow)
	}
	if c.Permissions.Auto.ClassifierModel != "" {
		t.Errorf("auto.classifier_model = %q, want empty (local loosening ignored)", c.Permissions.Auto.ClassifierModel)
	}
	if len(c.Permissions.Auto.Deny) != 0 || len(c.Permissions.Auto.Ask) != 0 {
		t.Errorf("auto block leaked from local: deny=%v ask=%v", c.Permissions.Auto.Deny, c.Permissions.Auto.Ask)
	}

	w := warnings()
	if w == "" {
		t.Fatal("expected a startup warning naming ignored keys, got none")
	}
	for _, key := range []string{"permissions.mode", "permissions.auto_approve", "permissions.session_allow", "permissions.auto"} {
		if !strings.Contains(w, key) {
			t.Errorf("warning %q does not mention %q", w, key)
		}
	}
	// Exactly one aggregated warning line.
	if got := strings.Count(strings.TrimRight(w, "\n"), "\n"); got != 0 {
		t.Errorf("expected one aggregated warning line, got %d newlines in %q", got, w)
	}
}

// TestTrustedFileMayLoosenPolicy asserts the same loosening keys ARE honored
// when they come from the trusted ~/.fuse/config.yml.
func TestTrustedFileMayLoosenPolicy(t *testing.T) {
	cwd := chdirTemp(t)
	home := filepath.Join(cwd, "home")
	if err := os.MkdirAll(filepath.Join(home, ".fuse"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	os.Unsetenv("LLM_GATEWAY_URL")
	os.Unsetenv("LLM_GATEWAY_KEY")

	trusted := `
permissions:
  mode: "off"
  auto_approve: ["bash:*"]
  session_allow: false
  auto:
    classifier_model: my-model
`
	if err := os.WriteFile(filepath.Join(home, ".fuse", "config.yml"), []byte(trusted), 0o644); err != nil {
		t.Fatal(err)
	}

	warnings := captureWarnings(t)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Permissions.Mode != "off" {
		t.Errorf("mode = %q, want off (trusted loosening honored)", c.Permissions.Mode)
	}
	if len(c.Permissions.AutoApprove) != 1 || c.Permissions.AutoApprove[0] != "bash:*" {
		t.Errorf("auto_approve = %v, want [bash:*]", c.Permissions.AutoApprove)
	}
	if c.Permissions.SessionAllow {
		t.Errorf("session_allow = %v, want false (trusted honored)", c.Permissions.SessionAllow)
	}
	if c.Permissions.Auto.ClassifierModel != "my-model" {
		t.Errorf("auto.classifier_model = %q, want my-model", c.Permissions.Auto.ClassifierModel)
	}
	if w := warnings(); w != "" {
		t.Errorf("trusted file must not warn, got %q", w)
	}
}

// TestLocalFileMayTightenPolicy asserts tightening keys (always_prompt,
// disabled) from the repo-plantable file ARE honored and do not warn.
func TestLocalFileMayTightenPolicy(t *testing.T) {
	cwd := chdirTemp(t)
	home := filepath.Join(cwd, "home")
	if err := os.MkdirAll(filepath.Join(home, ".fuse"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	os.Unsetenv("LLM_GATEWAY_URL")
	os.Unsetenv("LLM_GATEWAY_KEY")

	local := `
permissions:
  always_prompt: ["bash:git push *"]
  disabled: ["web_fetch"]
`
	if err := os.WriteFile(filepath.Join(cwd, ".fuse.local.yml"), []byte(local), 0o644); err != nil {
		t.Fatal(err)
	}

	warnings := captureWarnings(t)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Permissions.AlwaysPrompt) != 1 || c.Permissions.AlwaysPrompt[0] != "bash:git push *" {
		t.Errorf("always_prompt = %v, want [bash:git push *] (local tightening honored)", c.Permissions.AlwaysPrompt)
	}
	if len(c.Permissions.Disabled) != 1 || c.Permissions.Disabled[0] != "web_fetch" {
		t.Errorf("disabled = %v, want [web_fetch] (local tightening honored)", c.Permissions.Disabled)
	}
	if w := warnings(); w != "" {
		t.Errorf("tightening-only local file must not warn, got %q", w)
	}
}

func TestLoadEnvOverridesGateway(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("LLM_GATEWAY_URL", "http://env:9000/v1")
	t.Setenv("LLM_GATEWAY_KEY", "envkey")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Gateway.URL != "http://env:9000/v1" || c.Gateway.Key != "envkey" {
		t.Errorf("env override failed: %+v", c.Gateway)
	}
}
