package config

import (
	"os"
	"path/filepath"
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
