package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAddMCPServerRoundTrip(t *testing.T) {
	dir := t.TempDir()
	origHome := os.Getenv("HOME")
	t.Setenv("HOME", dir)
	defer os.Setenv("HOME", origHome)

	srv := MCPServerConfig{Name: "test-server", Transport: "stdio", Command: []string{"npx", "-y", "some-mcp"}}
	if err := AddMCPServer(srv); err != nil {
		t.Fatalf("AddMCPServer: %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load after add: %v", err)
	}
	var found bool
	for _, s := range cfg.MCPServers {
		if s.Name == "test-server" {
			found = true
			if s.Transport != "stdio" {
				t.Errorf("transport = %q, want stdio", s.Transport)
			}
		}
	}
	if !found {
		t.Error("added server not found in config")
	}
}

func TestAddMCPServerReplace(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	srv1 := MCPServerConfig{Name: "srv", Transport: "stdio", Command: []string{"cmd1"}}
	if err := AddMCPServer(srv1); err != nil {
		t.Fatalf("first add: %v", err)
	}

	srv2 := MCPServerConfig{Name: "srv", Transport: "http", URL: "http://localhost:8080"}
	if err := AddMCPServer(srv2); err != nil {
		t.Fatalf("replace: %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	count := 0
	for _, s := range cfg.MCPServers {
		if s.Name == "srv" {
			count++
			if s.Transport != "http" {
				t.Errorf("transport not updated: %q", s.Transport)
			}
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 server named 'srv', got %d", count)
	}
}

func TestRemoveMCPServer(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	for _, name := range []string{"alpha", "beta", "gamma"} {
		if err := AddMCPServer(MCPServerConfig{Name: name, Transport: "stdio", Command: []string{"cmd"}}); err != nil {
			t.Fatalf("add %s: %v", name, err)
		}
	}

	if err := RemoveMCPServer("beta"); err != nil {
		t.Fatalf("RemoveMCPServer: %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, s := range cfg.MCPServers {
		if s.Name == "beta" {
			t.Error("beta should have been removed")
		}
	}
	if len(cfg.MCPServers) != 2 {
		t.Errorf("want 2 servers after remove, got %d", len(cfg.MCPServers))
	}
}

func TestRemoveMCPServerNoop(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	// Remove from empty config is a no-op.
	if err := RemoveMCPServer("nonexistent"); err != nil {
		t.Errorf("RemoveMCPServer nonexistent: %v", err)
	}
}

// TestAddMCPServerPreservesOtherConfig is the regression test for the config
// writer data-loss bug: AddMCPServer/RemoveMCPServer must preserve every other
// key in the file (permissions, models, research, agents, etc.), rewriting only
// the mcp_servers list.
func TestAddMCPServerPreservesOtherConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	// Seed a config file rich in non-MCP keys, including a per-project trust
	// entry and a key the Config struct does not model at all, to prove the
	// writer round-trips the raw document rather than a typed subset.
	cfgPath := filepath.Join(dir, ".fuse", "config.yml")
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	seed := `gateway:
  url: http://gw.local
  key: secret
permissions:
  mode: prompt-all
models:
  default: fast
  entries:
    fast:
      id: cloud/whatever
      max_tokens: 4096
research:
  provider: brave
agents:
  max_spawns: 7
  max_concurrent: 3
future_unknown_key:
  nested: keep-me
mcp_servers:
  - name: existing
    transport: stdio
    command: [old]
`
	if err := os.WriteFile(cfgPath, []byte(seed), 0o644); err != nil {
		t.Fatalf("write seed: %v", err)
	}

	// Mutate the MCP list twice: add then remove.
	if err := AddMCPServer(MCPServerConfig{Name: "added", Transport: "stdio", Command: []string{"new"}}); err != nil {
		t.Fatalf("AddMCPServer: %v", err)
	}
	if err := RemoveMCPServer("existing"); err != nil {
		t.Fatalf("RemoveMCPServer: %v", err)
	}

	// Typed reload: non-MCP fields must survive.
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Permissions.Mode != "prompt-all" {
		t.Errorf("permissions.mode lost: got %q, want prompt-all", cfg.Permissions.Mode)
	}
	if cfg.Research.Provider != "brave" {
		t.Errorf("research.provider lost: got %q", cfg.Research.Provider)
	}
	if cfg.Agents.MaxSpawns != 7 {
		t.Errorf("agents.max_spawns lost: got %d, want 7", cfg.Agents.MaxSpawns)
	}
	if cfg.Gateway.Key != "secret" {
		t.Errorf("gateway.key lost: got %q", cfg.Gateway.Key)
	}
	if cfg.Gateway.URL != "http://gw.local" {
		t.Errorf("gateway.url lost: got %q", cfg.Gateway.URL)
	}

	// The MCP mutation itself must have taken effect.
	var haveAdded, haveExisting bool
	for _, s := range cfg.MCPServers {
		switch s.Name {
		case "added":
			haveAdded = true
		case "existing":
			haveExisting = true
		}
	}
	if !haveAdded {
		t.Error("added server missing after mutation")
	}
	if haveExisting {
		t.Error("removed server 'existing' still present")
	}

	// Raw-file check: the unknown key the Config struct never models must still
	// be on disk (proves document-level, not struct-level, round-trip).
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("reread config: %v", err)
	}
	if !strings.Contains(string(raw), "future_unknown_key") {
		t.Errorf("unmodeled key future_unknown_key was dropped; file:\n%s", raw)
	}
	if !strings.Contains(string(raw), "keep-me") {
		t.Errorf("unmodeled nested value keep-me was dropped; file:\n%s", raw)
	}
}

func TestSetModelRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	mc := ModelConfig{ID: "cloud/new-model", MaxTokens: 8192, ContextWindow: 131072, Persona: "coding"}
	if err := SetModel("shiny", mc); err != nil {
		t.Fatalf("SetModel: %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load after SetModel: %v", err)
	}
	got, ok := cfg.Models.Entries["shiny"]
	if !ok {
		t.Fatalf("alias 'shiny' not found; entries=%v", cfg.Models.Entries)
	}
	if got.ID != "cloud/new-model" || got.MaxTokens != 8192 || got.ContextWindow != 131072 || got.Persona != "coding" {
		t.Errorf("round-tripped model = %+v, want %+v", got, mc)
	}
}

func TestSetModelReplacesInPlace(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	if err := SetModel("m", ModelConfig{ID: "cloud/one", MaxTokens: 1}); err != nil {
		t.Fatalf("first SetModel: %v", err)
	}
	if err := SetModel("m", ModelConfig{ID: "cloud/two", MaxTokens: 2}); err != nil {
		t.Fatalf("replace SetModel: %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Models.Entries["m"]; got.ID != "cloud/two" || got.MaxTokens != 2 {
		t.Errorf("replaced model = %+v, want id=cloud/two max=2", got)
	}
	if len(cfg.Models.Entries) != 1 {
		t.Errorf("want exactly 1 alias, got %d: %v", len(cfg.Models.Entries), cfg.Models.Entries)
	}
}

func TestRemoveModel(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	for _, a := range []string{"alpha", "beta", "gamma"} {
		if err := SetModel(a, ModelConfig{ID: "cloud/" + a, MaxTokens: 1}); err != nil {
			t.Fatalf("set %s: %v", a, err)
		}
	}
	if err := RemoveModel("beta"); err != nil {
		t.Fatalf("RemoveModel: %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := cfg.Models.Entries["beta"]; ok {
		t.Error("beta should have been removed")
	}
	if len(cfg.Models.Entries) != 2 {
		t.Errorf("want 2 aliases after remove, got %d", len(cfg.Models.Entries))
	}
}

func TestRemoveModelNoop(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	// Remove from a config with no models block is a no-op.
	if err := RemoveModel("nonexistent"); err != nil {
		t.Errorf("RemoveModel on empty config: %v", err)
	}
}

// TestSetModelPreservesDefaultAndOtherKeys is the data-loss regression for the
// models writer: SetModel/RemoveModel must preserve models.default, sibling
// aliases, and every unrelated key — mutating only the one alias's sub-map.
func TestSetModelPreservesDefaultAndOtherKeys(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	cfgPath := filepath.Join(dir, ".fuse", "config.yml")
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Real on-disk shape: `default` and alias sub-maps are siblings under models.
	seed := `gateway:
  url: http://gw.local
  key: secret
permissions:
  mode: prompt-all
models:
  default: fast
  fast:
    id: cloud/fast
    max_tokens: 4096
  slow:
    id: cloud/slow
    max_tokens: 2048
future_unknown_key:
  nested: keep-me
`
	if err := os.WriteFile(cfgPath, []byte(seed), 0o644); err != nil {
		t.Fatalf("write seed: %v", err)
	}

	// Add a new alias and remove an existing one.
	if err := SetModel("added", ModelConfig{ID: "cloud/added", MaxTokens: 16384, Persona: "research"}); err != nil {
		t.Fatalf("SetModel: %v", err)
	}
	if err := RemoveModel("slow"); err != nil {
		t.Fatalf("RemoveModel: %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// models.default and the untouched alias survive.
	if cfg.Models.Default != "fast" {
		t.Errorf("models.default lost: got %q, want fast", cfg.Models.Default)
	}
	if _, ok := cfg.Models.Entries["fast"]; !ok {
		t.Error("sibling alias 'fast' dropped")
	}
	if _, ok := cfg.Models.Entries["slow"]; ok {
		t.Error("'slow' should have been removed")
	}
	if got, ok := cfg.Models.Entries["added"]; !ok || got.ID != "cloud/added" {
		t.Errorf("added alias missing or wrong: %+v ok=%v", got, ok)
	}
	// Unrelated keys survive.
	if cfg.Permissions.Mode != "prompt-all" {
		t.Errorf("permissions.mode lost: %q", cfg.Permissions.Mode)
	}
	if cfg.Gateway.Key != "secret" {
		t.Errorf("gateway.key lost: %q", cfg.Gateway.Key)
	}
	// Unmodeled key survives at the document level.
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("reread: %v", err)
	}
	if !strings.Contains(string(raw), "future_unknown_key") || !strings.Contains(string(raw), "keep-me") {
		t.Errorf("unmodeled key dropped; file:\n%s", raw)
	}
}

// TestSetModelCreatesMissingModelsBlock: writing an alias into a config that has
// no models block at all must synthesize the block rather than error.
func TestSetModelCreatesMissingModelsBlock(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	cfgPath := filepath.Join(dir, ".fuse", "config.yml")
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(cfgPath, []byte("permissions:\n  mode: smart\n"), 0o644); err != nil {
		t.Fatalf("write seed: %v", err)
	}

	if err := SetModel("first", ModelConfig{ID: "cloud/first", MaxTokens: 1024}); err != nil {
		t.Fatalf("SetModel: %v", err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, ok := cfg.Models.Entries["first"]; !ok || got.ID != "cloud/first" {
		t.Errorf("alias not written into synthesized models block: %+v ok=%v", got, ok)
	}
	if cfg.Permissions.Mode != "smart" {
		t.Errorf("permissions.mode lost: %q", cfg.Permissions.Mode)
	}
}

func TestWriteAtomic(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	srv := MCPServerConfig{Name: "atomic-test", Transport: "stdio", Command: []string{"cmd"}}
	if err := AddMCPServer(srv); err != nil {
		t.Fatalf("AddMCPServer: %v", err)
	}

	// Verify the file was written to the expected path.
	path := filepath.Join(dir, ".fuse", "config.yml")
	if _, err := os.Stat(path); err != nil {
		t.Errorf("config file not found at %s: %v", path, err)
	}
}
