package config

import (
	"bytes"
	"fmt"
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
	// MaxTurns default is now UNSET (nil): the context-aware backstop
	// (unlimited in the interactive shell, 100 headless) is applied at the
	// call site in cmd/fuse, not baked into a config default. See change 0038.
	if c.MaxTurns != nil {
		t.Errorf("MaxTurns default = %v, want nil (unset — resolved contextually)", *c.MaxTurns)
	}
}

func TestMaxTurnsPresenceDetection(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantNil bool
		wantVal int
	}{
		{"omitted stays unset", "gateway:\n  url: http://x\n", true, 0},
		{"explicit zero is unlimited", "max_turns: 0\n", false, 0},
		{"explicit positive caps", "max_turns: 7\n", false, 7},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			home := filepath.Join(dir, "home")
			if err := os.MkdirAll(filepath.Join(home, ".fuse"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(home, ".fuse", "config.yml"), []byte(tt.yaml), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Setenv("HOME", home)
			os.Unsetenv("LLM_GATEWAY_URL")
			os.Unsetenv("LLM_GATEWAY_KEY")

			c, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if tt.wantNil {
				if c.MaxTurns != nil {
					t.Fatalf("MaxTurns = %v, want nil (unset)", *c.MaxTurns)
				}
				return
			}
			if c.MaxTurns == nil {
				t.Fatalf("MaxTurns = nil, want explicit %d", tt.wantVal)
			}
			if *c.MaxTurns != tt.wantVal {
				t.Errorf("MaxTurns = %d, want %d", *c.MaxTurns, tt.wantVal)
			}
		})
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
	if c.Agents.MaxSpawns != 64 {
		t.Errorf("default Agents.MaxSpawns = %d, want 64", c.Agents.MaxSpawns)
	}
}

func TestDefaultAgentsMaxConcurrent(t *testing.T) {
	c := Default()
	if c.Agents.MaxConcurrent != 16 {
		t.Errorf("default Agents.MaxConcurrent = %d, want 16", c.Agents.MaxConcurrent)
	}
}

func TestLoadAgentsMaxConcurrentOverride(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(filepath.Join(home, ".fuse"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := "agents:\n  max_concurrent: 4\n"
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
	if c.Agents.MaxConcurrent != 4 {
		t.Errorf("Agents.MaxConcurrent = %d, want 4 (override)", c.Agents.MaxConcurrent)
	}
}

func TestLoadAgentsMaxConcurrentUnsetKeepsDefault(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(filepath.Join(home, ".fuse"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A config file that does NOT mention max_concurrent must keep the default.
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
	if c.Agents.MaxConcurrent != 16 {
		t.Errorf("Agents.MaxConcurrent = %d, want 16 (unset keeps default)", c.Agents.MaxConcurrent)
	}
}

func TestLoadAgentsMaxConcurrentNegativeClampsToDefault(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(filepath.Join(home, ".fuse"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A negative override passes the != 0 merge gate but is nonsensical: it
	// would silently disable the scheduler's active-child slot brake (guarded on
	// a positive cap) even though the tree clamps its own copy. The loader must
	// clamp it back to the default so the tree and predicate stay consistent.
	cfg := "agents:\n  max_concurrent: -3\n"
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
	if c.Agents.MaxConcurrent != 16 {
		t.Errorf("Agents.MaxConcurrent = %d, want 16 (negative clamped to default)", c.Agents.MaxConcurrent)
	}
}

func TestLoadAgentsMaxSpawnsNegativeClampsToDefault(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(filepath.Join(home, ".fuse"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A negative budget would disable the permanent budget brake (guarded on
	// max > 0); clamp it back to the default.
	cfg := "agents:\n  max_spawns: -5\n"
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
	if c.Agents.MaxSpawns != 64 {
		t.Errorf("Agents.MaxSpawns = %d, want 64 (negative clamped to default)", c.Agents.MaxSpawns)
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
	if c.Agents.MaxSpawns != 64 {
		t.Errorf("Agents.MaxSpawns = %d, want 64 (unset keeps default)", c.Agents.MaxSpawns)
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

// TestLocalFileFetchFloorTightens asserts that auto.fetch_deny / auto.fetch_ask
// in the repo-plantable (untrusted) .fuse.local.yml ARE honored (they are
// TIGHTENING host-glob floors, ADR-0006), while the loosening auto.* keys in the
// SAME file (classifier_model, deny, ask) are still dropped and reported as
// ignored under the "permissions.auto" aggregate.
func TestLocalFileFetchFloorTightens(t *testing.T) {
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
  auto:
    classifier_model: evil-model
    deny: ["bash:rm *"]
    fetch_deny: ["*.evil.com"]
    fetch_ask: ["staging.example.com"]
`
	if err := os.WriteFile(filepath.Join(cwd, ".fuse.local.yml"), []byte(local), 0o644); err != nil {
		t.Fatal(err)
	}

	warnings := captureWarnings(t)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}

	// Tightening host-glob floors are honored from the untrusted file.
	if len(c.Permissions.Auto.FetchDeny) != 1 || c.Permissions.Auto.FetchDeny[0] != "*.evil.com" {
		t.Errorf("auto.fetch_deny = %v, want [*.evil.com] (local tightening honored)", c.Permissions.Auto.FetchDeny)
	}
	if len(c.Permissions.Auto.FetchAsk) != 1 || c.Permissions.Auto.FetchAsk[0] != "staging.example.com" {
		t.Errorf("auto.fetch_ask = %v, want [staging.example.com] (local tightening honored)", c.Permissions.Auto.FetchAsk)
	}

	// Loosening auto.* keys in the same file are still dropped.
	if c.Permissions.Auto.ClassifierModel != "" {
		t.Errorf("auto.classifier_model = %q, want empty (local loosening ignored)", c.Permissions.Auto.ClassifierModel)
	}
	if len(c.Permissions.Auto.Deny) != 0 {
		t.Errorf("auto.deny = %v, want empty (local loosening ignored)", c.Permissions.Auto.Deny)
	}

	// The loosening block must be reported; the tightening keys must NOT.
	w := warnings()
	if !strings.Contains(w, "permissions.auto") {
		t.Errorf("warning %q must name the ignored loosening auto block", w)
	}
	if strings.Contains(w, "fetch_deny") || strings.Contains(w, "fetch_ask") {
		t.Errorf("warning %q must not name the honored tightening fetch keys", w)
	}
}

// TestTrustedFileFetchFloorHonored asserts the trusted ~/.fuse/config.yml honors
// the fetch floor alongside the loosening auto.* keys (all auto.* honored when
// trusted).
func TestTrustedFileFetchFloorHonored(t *testing.T) {
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
  auto:
    classifier_model: my-model
    fetch_deny: ["*.evil.com"]
    fetch_ask: ["staging.example.com"]
`
	if err := os.WriteFile(filepath.Join(home, ".fuse", "config.yml"), []byte(trusted), 0o644); err != nil {
		t.Fatal(err)
	}

	warnings := captureWarnings(t)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Permissions.Auto.ClassifierModel != "my-model" {
		t.Errorf("auto.classifier_model = %q, want my-model (trusted honored)", c.Permissions.Auto.ClassifierModel)
	}
	if len(c.Permissions.Auto.FetchDeny) != 1 || c.Permissions.Auto.FetchDeny[0] != "*.evil.com" {
		t.Errorf("auto.fetch_deny = %v, want [*.evil.com] (trusted honored)", c.Permissions.Auto.FetchDeny)
	}
	if len(c.Permissions.Auto.FetchAsk) != 1 || c.Permissions.Auto.FetchAsk[0] != "staging.example.com" {
		t.Errorf("auto.fetch_ask = %v, want [staging.example.com] (trusted honored)", c.Permissions.Auto.FetchAsk)
	}
	if w := warnings(); w != "" {
		t.Errorf("trusted file must not warn, got %q", w)
	}
}

// canon canonicalizes a path via EvalSymlinks so expected project keys match
// the loader's canonicalization (macOS /var → /private/var).
func canon(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", p, err)
	}
	return r
}

// writeHomeConfigAt writes ~/.fuse/config.yml under a caller-chosen home dir
// (used where cwd must sit under a project key, unlike research_test's helper
// which owns its own temp home).
func writeHomeConfigAt(t *testing.T, home, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(home, ".fuse"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".fuse", "config.yml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestProjectOverrideExactMatch — a project key equal to the canonical cwd
// overrides a global permissions.mode.
func TestProjectOverrideExactMatch(t *testing.T) {
	cwd := canon(t, chdirTemp(t))
	home := filepath.Join(cwd, "home")
	t.Setenv("HOME", home)
	os.Unsetenv("LLM_GATEWAY_URL")
	os.Unsetenv("LLM_GATEWAY_KEY")

	cfg := "permissions:\n  mode: prompt-all\nprojects:\n  " + cwd + ":\n    permissions:\n      mode: auto\n"
	writeHomeConfigAt(t, home, cfg)

	warnings := captureWarnings(t)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Permissions.Mode != "auto" {
		t.Errorf("mode = %q, want auto (exact project override)", c.Permissions.Mode)
	}
	if w := warnings(); w != "" {
		t.Errorf("trusted project override must not warn, got %q", w)
	}
}

// TestProjectOverrideAncestorMatch — a parent dir of cwd matches.
func TestProjectOverrideAncestorMatch(t *testing.T) {
	base := canon(t, chdirTemp(t))
	sub := filepath.Join(base, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	orig, _ := os.Getwd()
	if err := os.Chdir(sub); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(orig) })

	home := filepath.Join(base, "home")
	t.Setenv("HOME", home)
	os.Unsetenv("LLM_GATEWAY_URL")
	os.Unsetenv("LLM_GATEWAY_KEY")

	cfg := "permissions:\n  mode: prompt-all\nprojects:\n  " + base + ":\n    permissions:\n      mode: auto\n"
	writeHomeConfigAt(t, home, cfg)

	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Permissions.Mode != "auto" {
		t.Errorf("mode = %q, want auto (ancestor project override)", c.Permissions.Mode)
	}
}

// TestProjectOverrideLongestWins — two ancestor keys /p and /p/sub, cwd under
// /p/sub ⇒ /p/sub wins.
func TestProjectOverrideLongestWins(t *testing.T) {
	base := canon(t, chdirTemp(t))
	sub := filepath.Join(base, "sub")
	deep := filepath.Join(sub, "deep")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	orig, _ := os.Getwd()
	if err := os.Chdir(deep); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(orig) })

	home := filepath.Join(base, "home")
	t.Setenv("HOME", home)
	os.Unsetenv("LLM_GATEWAY_URL")
	os.Unsetenv("LLM_GATEWAY_KEY")

	cfg := "permissions:\n  mode: prompt-all\nprojects:\n" +
		"  " + base + ":\n    permissions:\n      mode: smart\n" +
		"  " + sub + ":\n    permissions:\n      mode: auto\n"
	writeHomeConfigAt(t, home, cfg)

	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Permissions.Mode != "auto" {
		t.Errorf("mode = %q, want auto (longest key /sub wins)", c.Permissions.Mode)
	}
}

// TestProjectOverrideSegmentGuard — a key …/b must NOT match a cwd under …/bc.
func TestProjectOverrideSegmentGuard(t *testing.T) {
	base := canon(t, chdirTemp(t))
	keyDir := filepath.Join(base, "b")  // the project key
	cwdDir := filepath.Join(base, "bc") // cwd — shares a prefix but not a segment
	if err := os.MkdirAll(keyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cwdDir, 0o755); err != nil {
		t.Fatal(err)
	}
	orig, _ := os.Getwd()
	if err := os.Chdir(cwdDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(orig) })

	home := filepath.Join(base, "home")
	t.Setenv("HOME", home)
	os.Unsetenv("LLM_GATEWAY_URL")
	os.Unsetenv("LLM_GATEWAY_KEY")

	cfg := "permissions:\n  mode: prompt-all\nprojects:\n  " + keyDir + ":\n    permissions:\n      mode: auto\n"
	writeHomeConfigAt(t, home, cfg)

	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Permissions.Mode != "prompt-all" {
		t.Errorf("mode = %q, want prompt-all (/b must not match cwd under /bc)", c.Permissions.Mode)
	}
}

// TestProjectOverrideSymlinkedCwd — cwd reached through a symlink whose
// EvalSymlinks target equals a project key ⇒ match.
func TestProjectOverrideSymlinkedCwd(t *testing.T) {
	base := canon(t, chdirTemp(t))
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink unsupported on this platform: %v", err)
	}
	orig, _ := os.Getwd()
	if err := os.Chdir(link); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(orig) })

	home := filepath.Join(base, "home")
	t.Setenv("HOME", home)
	os.Unsetenv("LLM_GATEWAY_URL")
	os.Unsetenv("LLM_GATEWAY_KEY")

	// Key is the REAL (canonical) target, not the symlink path.
	cfg := "permissions:\n  mode: prompt-all\nprojects:\n  " + real + ":\n    permissions:\n      mode: auto\n"
	writeHomeConfigAt(t, home, cfg)

	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Permissions.Mode != "auto" {
		t.Errorf("mode = %q, want auto (symlinked cwd resolves to key)", c.Permissions.Mode)
	}
}

// TestProjectOverrideNoMatch — cwd under no project key ⇒ global unchanged, no
// warning. The key is a REAL, resolvable sibling dir (not cwd, not an ancestor
// of it), so this exercises the resolved-but-non-ancestor branch rather than the
// unresolvable-key skip.
func TestProjectOverrideNoMatch(t *testing.T) {
	cwd := canon(t, chdirTemp(t))
	home := filepath.Join(cwd, "home")
	t.Setenv("HOME", home)
	os.Unsetenv("LLM_GATEWAY_URL")
	os.Unsetenv("LLM_GATEWAY_KEY")

	// A real sibling dir that resolves but is not an ancestor of cwd.
	sibling := canon(t, t.TempDir())
	cfg := "permissions:\n  mode: prompt-all\nprojects:\n  " + sibling + ":\n    permissions:\n      mode: auto\n"
	writeHomeConfigAt(t, home, cfg)

	warnings := captureWarnings(t)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Permissions.Mode != "prompt-all" {
		t.Errorf("mode = %q, want prompt-all (no project matched)", c.Permissions.Mode)
	}
	if w := warnings(); w != "" {
		t.Errorf("no-match must not warn, got %q", w)
	}
}

// TestProjectOverrideLocalFileIgnoredAndWarned — a projects: block in the
// repo-plantable .fuse.local.yml is ignored and the aggregated warning names
// projects, in a single line (like TestLocalFileCannotLoosenPolicy).
func TestProjectOverrideLocalFileIgnoredAndWarned(t *testing.T) {
	cwd := canon(t, chdirTemp(t))
	home := filepath.Join(cwd, "home")
	if err := os.MkdirAll(filepath.Join(home, ".fuse"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	os.Unsetenv("LLM_GATEWAY_URL")
	os.Unsetenv("LLM_GATEWAY_KEY")

	local := "projects:\n  " + cwd + ":\n    permissions:\n      mode: auto\n"
	if err := os.WriteFile(filepath.Join(cwd, ".fuse.local.yml"), []byte(local), 0o644); err != nil {
		t.Fatal(err)
	}

	warnings := captureWarnings(t)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Permissions.Mode != "smart" {
		t.Errorf("mode = %q, want smart (local projects: ignored)", c.Permissions.Mode)
	}
	w := warnings()
	if !strings.Contains(w, "projects") {
		t.Errorf("warning %q does not mention projects", w)
	}
	if got := strings.Count(strings.TrimRight(w, "\n"), "\n"); got != 0 {
		t.Errorf("expected one aggregated warning line, got %d newlines in %q", got, w)
	}
}

// TestProjectOverrideFullSubtree — a project entry sets auto.classifier_model
// and auto.deny/auto.ask; all resolve (proving the whole auto.* subtree merges,
// not just mode).
func TestProjectOverrideFullSubtree(t *testing.T) {
	cwd := canon(t, chdirTemp(t))
	home := filepath.Join(cwd, "home")
	t.Setenv("HOME", home)
	os.Unsetenv("LLM_GATEWAY_URL")
	os.Unsetenv("LLM_GATEWAY_KEY")

	cfg := "projects:\n  " + cwd + ":\n" +
		"    permissions:\n" +
		"      mode: auto\n" +
		"      auto:\n" +
		"        classifier_model: proj-model\n" +
		"        deny: [\"bash:rm *\"]\n" +
		"        ask: [\"bash:curl *\"]\n"
	writeHomeConfigAt(t, home, cfg)

	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Permissions.Mode != "auto" {
		t.Errorf("mode = %q, want auto", c.Permissions.Mode)
	}
	if c.Permissions.Auto.ClassifierModel != "proj-model" {
		t.Errorf("Auto.ClassifierModel = %q, want proj-model", c.Permissions.Auto.ClassifierModel)
	}
	if len(c.Permissions.Auto.Deny) != 1 || c.Permissions.Auto.Deny[0] != "bash:rm *" {
		t.Errorf("Auto.Deny = %v, want [bash:rm *]", c.Permissions.Auto.Deny)
	}
	if len(c.Permissions.Auto.Ask) != 1 || c.Permissions.Auto.Ask[0] != "bash:curl *" {
		t.Errorf("Auto.Ask = %v, want [bash:curl *]", c.Permissions.Auto.Ask)
	}
}

// TestProjectOverrideStillOverridableByLocalTighten — a project mode: auto is
// still tightenable by a later .fuse.local.yml (precedence: project < local
// tighten).
func TestProjectOverrideStillOverridableByLocalTighten(t *testing.T) {
	cwd := canon(t, chdirTemp(t))
	home := filepath.Join(cwd, "home")
	t.Setenv("HOME", home)
	os.Unsetenv("LLM_GATEWAY_URL")
	os.Unsetenv("LLM_GATEWAY_KEY")

	cfg := "projects:\n  " + cwd + ":\n    permissions:\n      mode: auto\n"
	writeHomeConfigAt(t, home, cfg)

	local := "permissions:\n  disabled: [\"web_fetch\"]\n"
	if err := os.WriteFile(filepath.Join(cwd, ".fuse.local.yml"), []byte(local), 0o644); err != nil {
		t.Fatal(err)
	}

	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Permissions.Mode != "auto" {
		t.Errorf("mode = %q, want auto (project override survives local tighten)", c.Permissions.Mode)
	}
	if len(c.Permissions.Disabled) != 1 || c.Permissions.Disabled[0] != "web_fetch" {
		t.Errorf("disabled = %v, want [web_fetch] (local tighten applied over project)", c.Permissions.Disabled)
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

// --- change 0034: workflows: config surface ---

// The research workflow ships as a built-in default (change 0034): it binds the
// research skill to a facet-researcher worker and the {5,8,1} pool, so /research
// is policy-governed out of the box. Config may override it via the normal merge.
func TestDefaultShipsResearchWorkflow(t *testing.T) {
	c := Default()
	wf, ok := c.Workflows["research"]
	if !ok {
		t.Fatalf("Default().Workflows missing 'research'; got %v", c.Workflows)
	}
	if wf.Skill != "research" {
		t.Errorf("skill = %q, want research", wf.Skill)
	}
	if wf.Pool.Concurrent != 5 || wf.Pool.Total != 8 || wf.Pool.MaxDepth != 1 {
		t.Errorf("pool = %+v, want {5 8 1}", wf.Pool)
	}
	w, ok := wf.Workers["facet-researcher"]
	if !ok {
		t.Fatalf("facet-researcher worker missing; got %v", wf.Workers)
	}
	for _, tl := range w.Tools {
		if tl == "spawn_agent" {
			t.Error("facet-researcher must not carry spawn_agent (cannot nest)")
		}
	}
	want := map[string]bool{"web_search": true, "web_fetch": true, "read_file": true}
	if len(w.Tools) != len(want) {
		t.Errorf("facet-researcher tools = %v, want %v", w.Tools, want)
	}
	for _, tl := range w.Tools {
		if !want[tl] {
			t.Errorf("unexpected tool %q in facet-researcher", tl)
		}
	}
}

func TestLoadWorkflowsParsesAndLayers(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(filepath.Join(home, ".fuse"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := `
workflows:
  research:
    skill: research
    pool:
      concurrent: 5
      total: 8
      max_depth: 1
    workers:
      facet-researcher:
        tools: [web_search, web_fetch, read_file]
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
	wf, ok := c.Workflows["research"]
	if !ok {
		t.Fatalf("Workflows[research] missing; got %v", c.Workflows)
	}
	if wf.Skill != "research" {
		t.Errorf("skill = %q, want research", wf.Skill)
	}
	if wf.Pool.Concurrent != 5 || wf.Pool.Total != 8 || wf.Pool.MaxDepth != 1 {
		t.Errorf("pool = %+v, want {5 8 1}", wf.Pool)
	}
	w, ok := wf.Workers["facet-researcher"]
	if !ok {
		t.Fatalf("worker facet-researcher missing; got %v", wf.Workers)
	}
	if got := strings.Join(w.Tools, ","); got != "web_search,web_fetch,read_file" {
		t.Errorf("worker tools = %q", got)
	}
	for _, tl := range w.Tools {
		if tl == "spawn_agent" {
			t.Error("facet-researcher must not carry spawn_agent")
		}
	}
}

// TestLocalWorkflowsTightenOnly asserts the ADR-0006 trust boundary for the
// repo-plantable .fuse.local.yml: it may LOWER a workflow pool number (tighten)
// but a value that would LOOSEN it (raise the cap) is ignored, keeping the
// trusted home-file value, and a warning names the workflow.
func TestLocalWorkflowsTightenOnly(t *testing.T) {
	cwd := chdirTemp(t)
	home := filepath.Join(cwd, "home")
	if err := os.MkdirAll(filepath.Join(home, ".fuse"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	os.Unsetenv("LLM_GATEWAY_URL")
	os.Unsetenv("LLM_GATEWAY_KEY")

	// Trusted home file establishes the baseline pool.
	trusted := `
workflows:
  research:
    skill: research
    pool: {concurrent: 5, total: 8, max_depth: 1}
`
	if err := os.WriteFile(filepath.Join(home, ".fuse", "config.yml"), []byte(trusted), 0o644); err != nil {
		t.Fatal(err)
	}
	// Untrusted local file tries to LOOSEN total (8 -> 64) and TIGHTEN concurrent (5 -> 2).
	local := `
workflows:
  research:
    pool: {concurrent: 2, total: 64}
`
	if err := os.WriteFile(filepath.Join(cwd, ".fuse.local.yml"), []byte(local), 0o644); err != nil {
		t.Fatal(err)
	}

	warnings := captureWarnings(t)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	wf := c.Workflows["research"]
	if wf.Pool.Concurrent != 2 {
		t.Errorf("concurrent = %d, want 2 (local tighten honored)", wf.Pool.Concurrent)
	}
	if wf.Pool.Total != 8 {
		t.Errorf("total = %d, want 8 (local loosening ignored)", wf.Pool.Total)
	}
	if w := warnings(); !strings.Contains(w, "research") {
		t.Errorf("expected a warning naming the loosened workflow; got %q", w)
	}
}

// --- change 0036: queue_bound + throughput: + pool.tokens config surface ---

// TestLoadFullThroughputSketchRoundTrips parses the spec's full config sketch
// and asserts every new knob resolves: queue_bound, throughput.{rpm,tpm,session}
// with a per-provider override, and workflows.<name>.pool.tokens.
func TestLoadFullThroughputSketchRoundTrips(t *testing.T) {
	c := writeHomeConfig(t, `
agents:
  max_concurrent: 16
  max_spawns: 64
  queue_bound: 2.0
throughput:
  requests_per_minute: 120
  tokens_per_minute: 90000
  session_tokens: 1000000
  providers:
    deepseek:
      requests_per_minute: 60
      tokens_per_minute: 40000
workflows:
  research:
    skill: research
    pool: {concurrent: 5, total: 8, max_depth: 1, tokens: 500000}
`)

	if c.Agents.QueueBound != 2.0 {
		t.Errorf("Agents.QueueBound = %v, want 2.0", c.Agents.QueueBound)
	}
	if c.Throughput.RequestsPerMinute != 120 {
		t.Errorf("Throughput.RequestsPerMinute = %d, want 120", c.Throughput.RequestsPerMinute)
	}
	if c.Throughput.TokensPerMinute != 90000 {
		t.Errorf("Throughput.TokensPerMinute = %d, want 90000", c.Throughput.TokensPerMinute)
	}
	if c.Throughput.SessionTokens != 1000000 {
		t.Errorf("Throughput.SessionTokens = %d, want 1000000", c.Throughput.SessionTokens)
	}
	dp, ok := c.Throughput.Providers["deepseek"]
	if !ok {
		t.Fatalf("Throughput.Providers missing deepseek; got %v", c.Throughput.Providers)
	}
	if dp.RequestsPerMinute != 60 {
		t.Errorf("deepseek RequestsPerMinute = %d, want 60", dp.RequestsPerMinute)
	}
	if dp.TokensPerMinute != 40000 {
		t.Errorf("deepseek TokensPerMinute = %d, want 40000", dp.TokensPerMinute)
	}
	if c.Workflows["research"].Pool.Tokens != 500000 {
		t.Errorf("research pool.tokens = %d, want 500000", c.Workflows["research"].Pool.Tokens)
	}
}

// TestAbsentThroughputAndQueueBoundAreZero asserts a config that mentions none
// of the new knobs leaves them at their zero values (Acceptance 5 tail: absent
// config ⇒ byte-identical behavior). QueueBound 0 = unset (default 2.0 applied
// where consumed), rpm/tpm/session/tokens 0 = unlimited/unset.
func TestAbsentThroughputAndQueueBoundAreZero(t *testing.T) {
	c := writeHomeConfig(t, "max_turns: 30\n")

	if c.Agents.QueueBound != 0 {
		t.Errorf("Agents.QueueBound = %v, want 0 (unset)", c.Agents.QueueBound)
	}
	if c.Throughput.RequestsPerMinute != 0 || c.Throughput.TokensPerMinute != 0 || c.Throughput.SessionTokens != 0 {
		t.Errorf("Throughput = %+v, want all-zero (unset)", c.Throughput)
	}
	if len(c.Throughput.Providers) != 0 {
		t.Errorf("Throughput.Providers = %v, want empty (unset)", c.Throughput.Providers)
	}
	// The built-in research workflow ships without a tokens quota.
	if c.Workflows["research"].Pool.Tokens != 0 {
		t.Errorf("default research pool.tokens = %d, want 0 (unset)", c.Workflows["research"].Pool.Tokens)
	}
}

// TestLoadQueueBoundOverride asserts a trusted home file sets queue_bound.
func TestLoadQueueBoundOverride(t *testing.T) {
	c := writeHomeConfig(t, "agents:\n  queue_bound: 3.5\n")
	if c.Agents.QueueBound != 3.5 {
		t.Errorf("Agents.QueueBound = %v, want 3.5", c.Agents.QueueBound)
	}
}

// TestLocalQueueBoundTightenOnly asserts .fuse.local.yml may LOWER queue_bound
// (tighten) but not raise it, mirroring the pool-number rule: a tightening is
// honored only when the current value is already set (>0) and the new value is a
// positive number strictly lower than it.
func TestLocalQueueBoundTightenOnly(t *testing.T) {
	cases := []struct {
		name     string
		home     float64
		local    float64
		want     float64
		wantWarn bool
	}{
		{"lower is honored", 4.0, 2.0, 2.0, false},
		{"raise is ignored", 2.0, 8.0, 2.0, true},
		{"set-from-unset is ignored", 0, 3.0, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cwd := chdirTemp(t)
			home := filepath.Join(cwd, "home")
			if err := os.MkdirAll(filepath.Join(home, ".fuse"), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("HOME", home)
			os.Unsetenv("LLM_GATEWAY_URL")
			os.Unsetenv("LLM_GATEWAY_KEY")

			trusted := ""
			if tc.home != 0 {
				trusted = fmt.Sprintf("agents:\n  queue_bound: %v\n", tc.home)
			}
			if trusted != "" {
				if err := os.WriteFile(filepath.Join(home, ".fuse", "config.yml"), []byte(trusted), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			local := fmt.Sprintf("agents:\n  queue_bound: %v\n", tc.local)
			if err := os.WriteFile(filepath.Join(cwd, ".fuse.local.yml"), []byte(local), 0o644); err != nil {
				t.Fatal(err)
			}

			warnings := captureWarnings(t)
			c, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if c.Agents.QueueBound != tc.want {
				t.Errorf("QueueBound = %v, want %v", c.Agents.QueueBound, tc.want)
			}
			w := warnings()
			if tc.wantWarn && !strings.Contains(w, "queue_bound") {
				t.Errorf("expected a warning naming queue_bound; got %q", w)
			}
			if !tc.wantWarn && w != "" {
				t.Errorf("tightening/no-op must not warn; got %q", w)
			}
		})
	}
}

// TestLocalThroughputTightenOnly asserts the 0=unlimited numerics on the global
// throughput block follow the same rule: a local file may only LOWER an
// already-set positive limit; raising it, or setting a previously-unset
// (0=unlimited) limit, is ignored — because for these axes 0 means "no limit",
// so introducing a limit or lowering one is stricter but the conservative rule
// (matching the pool-number merge) honors ONLY a strictly-lower positive value.
func TestLocalThroughputTightenOnly(t *testing.T) {
	cwd := chdirTemp(t)
	home := filepath.Join(cwd, "home")
	if err := os.MkdirAll(filepath.Join(home, ".fuse"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	os.Unsetenv("LLM_GATEWAY_URL")
	os.Unsetenv("LLM_GATEWAY_KEY")

	trusted := `
throughput:
  requests_per_minute: 100
  tokens_per_minute: 50000
  session_tokens: 200000
`
	if err := os.WriteFile(filepath.Join(home, ".fuse", "config.yml"), []byte(trusted), 0o644); err != nil {
		t.Fatal(err)
	}
	// Local: lower rpm (honored), raise tpm (ignored), keep session unset here.
	local := `
throughput:
  requests_per_minute: 60
  tokens_per_minute: 90000
`
	if err := os.WriteFile(filepath.Join(cwd, ".fuse.local.yml"), []byte(local), 0o644); err != nil {
		t.Fatal(err)
	}

	warnings := captureWarnings(t)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Throughput.RequestsPerMinute != 60 {
		t.Errorf("rpm = %d, want 60 (local lower honored)", c.Throughput.RequestsPerMinute)
	}
	if c.Throughput.TokensPerMinute != 50000 {
		t.Errorf("tpm = %d, want 50000 (local raise ignored)", c.Throughput.TokensPerMinute)
	}
	if c.Throughput.SessionTokens != 200000 {
		t.Errorf("session_tokens = %d, want 200000 (untouched)", c.Throughput.SessionTokens)
	}
	if w := warnings(); !strings.Contains(w, "throughput") {
		t.Errorf("expected a warning naming throughput; got %q", w)
	}
}

// TestTrustedThroughputNegativeIsDropped asserts a trusted home file's negative
// throughput axis is dropped rather than propagated (a negative would DISABLE
// the axis — 0 = unlimited — which is never a valid tightening). Mirrors the
// MaxSpawns/MaxConcurrent negative clamp a few lines up in mergeFile. Each case
// loads a home file whose axis is negative and asserts the result is 0 (unset),
// never a negative that would silently overwrite/disable a set positive.
func TestTrustedThroughputNegativeIsDropped(t *testing.T) {
	cases := []struct {
		name string
		axis string
		read func(Config) int
	}{
		{"rpm", "requests_per_minute", func(c Config) int { return c.Throughput.RequestsPerMinute }},
		{"tpm", "tokens_per_minute", func(c Config) int { return c.Throughput.TokensPerMinute }},
		{"session", "session_tokens", func(c Config) int { return c.Throughput.SessionTokens }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := writeHomeConfig(t, fmt.Sprintf("throughput:\n  %s: -5\n", tc.axis))
			if got := tc.read(c); got != 0 {
				t.Errorf("%s = %d, want 0 (trusted negative dropped, axis stays unset)", tc.axis, got)
			}
		})
	}
}

// TestLocalPoolTokensTightenOnly asserts workflows.<name>.pool.tokens follows
// the same tighten-only merge as the other pool numbers: local may lower an
// already-set quota but not raise it or set a previously-unset one.
func TestLocalPoolTokensTightenOnly(t *testing.T) {
	cwd := chdirTemp(t)
	home := filepath.Join(cwd, "home")
	if err := os.MkdirAll(filepath.Join(home, ".fuse"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	os.Unsetenv("LLM_GATEWAY_URL")
	os.Unsetenv("LLM_GATEWAY_KEY")

	trusted := `
workflows:
  research:
    skill: research
    pool: {concurrent: 5, total: 8, max_depth: 1, tokens: 500000}
`
	if err := os.WriteFile(filepath.Join(home, ".fuse", "config.yml"), []byte(trusted), 0o644); err != nil {
		t.Fatal(err)
	}
	// Local: lower tokens (honored), raise total (ignored).
	local := `
workflows:
  research:
    pool: {tokens: 100000, total: 64}
`
	if err := os.WriteFile(filepath.Join(cwd, ".fuse.local.yml"), []byte(local), 0o644); err != nil {
		t.Fatal(err)
	}

	warnings := captureWarnings(t)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	wf := c.Workflows["research"]
	if wf.Pool.Tokens != 100000 {
		t.Errorf("pool.tokens = %d, want 100000 (local lower honored)", wf.Pool.Tokens)
	}
	if wf.Pool.Total != 8 {
		t.Errorf("pool.total = %d, want 8 (local raise ignored)", wf.Pool.Total)
	}
	if w := warnings(); !strings.Contains(w, "research") {
		t.Errorf("expected a warning naming the loosened workflow; got %q", w)
	}
}

// --- change 0027: context.summarization config surface ---

// loadHomeConfig writes cfg as ~/.fuse/config.yml under a temp HOME and loads.
func loadHomeConfig(t *testing.T, cfg string) Config {
	t.Helper()
	home := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(filepath.Join(home, ".fuse"), 0o755); err != nil {
		t.Fatal(err)
	}
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
	return c
}

func TestLoadContextSummarizationFull(t *testing.T) {
	c := loadHomeConfig(t, "context:\n  summarization:\n    enabled: true\n    model: \"cheap/model\"\n    threshold: 0.85\n    max_output: 2000\n")
	s := c.Context.Summarization
	if !s.Enabled {
		t.Errorf("Enabled = false, want true")
	}
	if s.Model != "cheap/model" {
		t.Errorf("Model = %q, want cheap/model", s.Model)
	}
	if s.Threshold != 0.85 {
		t.Errorf("Threshold = %v, want 0.85", s.Threshold)
	}
	if s.MaxOutput != 2000 {
		t.Errorf("MaxOutput = %d, want 2000", s.MaxOutput)
	}
}

func TestLoadContextSummarizationAbsentDefaults(t *testing.T) {
	// The block is absent entirely: Tier 2 defaults on, with documented defaults.
	c := loadHomeConfig(t, "max_turns: 30\n")
	s := c.Context.Summarization
	if !s.Enabled {
		t.Errorf("Enabled = false, want true (default on when block absent)")
	}
	if s.Model != "" {
		t.Errorf("Model = %q, want \"\" (default = main model)", s.Model)
	}
	if s.Threshold != 0.85 {
		t.Errorf("Threshold = %v, want 0.85 (default)", s.Threshold)
	}
	if s.MaxOutput != 2000 {
		t.Errorf("MaxOutput = %d, want 2000 (default)", s.MaxOutput)
	}
}

func TestLoadContextSummarizationDisabled(t *testing.T) {
	c := loadHomeConfig(t, "context:\n  summarization:\n    enabled: false\n")
	if c.Context.Summarization.Enabled {
		t.Errorf("Enabled = true, want false (explicit disable)")
	}
	// Other fields keep their defaults even when only enabled is set.
	if c.Context.Summarization.Threshold != 0.85 {
		t.Errorf("Threshold = %v, want 0.85 (default retained)", c.Context.Summarization.Threshold)
	}
	if c.Context.Summarization.MaxOutput != 2000 {
		t.Errorf("MaxOutput = %d, want 2000 (default retained)", c.Context.Summarization.MaxOutput)
	}
}

func TestLoadContextSummarizationOnlyModel(t *testing.T) {
	// Only model set: the other fields keep their defaults.
	c := loadHomeConfig(t, "context:\n  summarization:\n    model: \"alias/x\"\n")
	s := c.Context.Summarization
	if s.Model != "alias/x" {
		t.Errorf("Model = %q, want alias/x", s.Model)
	}
	if !s.Enabled {
		t.Errorf("Enabled = false, want true (default retained)")
	}
	if s.Threshold != 0.85 {
		t.Errorf("Threshold = %v, want 0.85 (default retained)", s.Threshold)
	}
	if s.MaxOutput != 2000 {
		t.Errorf("MaxOutput = %d, want 2000 (default retained)", s.MaxOutput)
	}
}

func TestDefaultContextSummarizationOn(t *testing.T) {
	// Default() (no file at all) also yields Tier 2 on with defaults.
	s := Default().Context.Summarization
	if !s.Enabled || s.Threshold != 0.85 || s.MaxOutput != 2000 {
		t.Errorf("Default context.summarization = %+v, want {Enabled:true Threshold:0.85 MaxOutput:2000}", s)
	}
}

// --- change 0028: context.relevance config surface ---

func TestLoadContextRelevanceAbsentDefaults(t *testing.T) {
	c := loadHomeConfig(t, "max_turns: 30\n")
	r := c.Context.Relevance
	if !r.Heuristic {
		t.Errorf("Heuristic = false, want true (default on when block absent)")
	}
	if r.RecencyFloorPct != 50 {
		t.Errorf("RecencyFloorPct = %d, want 50", r.RecencyFloorPct)
	}
	if r.BodyScanBytes != 2048 {
		t.Errorf("BodyScanBytes = %d, want 2048", r.BodyScanBytes)
	}
	if r.ClassifierModel != "" {
		t.Errorf("ClassifierModel = %q, want \"\"", r.ClassifierModel)
	}
	if r.ClassifierBatchSize != 10 {
		t.Errorf("ClassifierBatchSize = %d, want 10", r.ClassifierBatchSize)
	}
	if r.BorderlineLo != 0.30 || r.BorderlineHi != 0.60 {
		t.Errorf("Borderline = [%v,%v], want [0.30,0.60]", r.BorderlineLo, r.BorderlineHi)
	}
}

func TestLoadContextRelevanceHeuristicDisabled(t *testing.T) {
	c := loadHomeConfig(t, "context:\n  relevance:\n    heuristic: false\n")
	if c.Context.Relevance.Heuristic {
		t.Errorf("Heuristic = true, want false (explicit disable)")
	}
	// Other fields keep their defaults.
	if c.Context.Relevance.RecencyFloorPct != 50 {
		t.Errorf("RecencyFloorPct = %d, want 50 (default retained)", c.Context.Relevance.RecencyFloorPct)
	}
}

func TestLoadContextRelevanceOverrides(t *testing.T) {
	yaml := "context:\n  relevance:\n" +
		"    recency_floor_pct: 30\n" +
		"    body_scan_bytes: 4096\n" +
		"    classifier_model: \"alias/classifier\"\n" +
		"    classifier_batch_size: 5\n" +
		"    borderline_lo: 0.2\n" +
		"    borderline_hi: 0.7\n"
	r := loadHomeConfig(t, yaml).Context.Relevance
	if r.RecencyFloorPct != 30 || r.BodyScanBytes != 4096 || r.ClassifierModel != "alias/classifier" ||
		r.ClassifierBatchSize != 5 || r.BorderlineLo != 0.2 || r.BorderlineHi != 0.7 {
		t.Errorf("overrides not applied: %+v", r)
	}
	if !r.Heuristic {
		t.Errorf("Heuristic = false, want true (default retained when unset)")
	}
}

func TestDefaultContextRelevanceOn(t *testing.T) {
	r := Default().Context.Relevance
	if !r.Heuristic || r.RecencyFloorPct != 50 || r.BodyScanBytes != 2048 ||
		r.ClassifierBatchSize != 10 || r.BorderlineLo != 0.30 || r.BorderlineHi != 0.60 {
		t.Errorf("Default context.relevance = %+v, want heuristic on with documented defaults", r)
	}
}

// --- change 0026: pipeline.synthesis caps config surface ---

// TestDefaultPipelineSynthesisCaps asserts the conservative built-in defaults
// for the pipeline synthesis caps are present after Default().
func TestDefaultPipelineSynthesisCaps(t *testing.T) {
	s := Default().Pipeline.Synthesis
	if s.MaxSteps != 50 {
		t.Errorf("MaxSteps = %d, want 50", s.MaxSteps)
	}
	if s.MaxFanout != 10 {
		t.Errorf("MaxFanout = %d, want 10", s.MaxFanout)
	}
	if s.MaxDepth != 4 {
		t.Errorf("MaxDepth = %d, want 4", s.MaxDepth)
	}
	if s.MaxAttempts != 3 {
		t.Errorf("MaxAttempts = %d, want 3", s.MaxAttempts)
	}
}

// TestLoadPipelineSynthesisBlock asserts the pipeline: { synthesis: {...} }
// block unmarshals from a trusted home config and overrides the defaults.
func TestLoadPipelineSynthesisBlock(t *testing.T) {
	c := loadHomeConfig(t, `
pipeline:
  synthesis:
    max_steps: 20
    max_fanout: 6
    max_depth: 3
    max_attempts: 2
`)
	s := c.Pipeline.Synthesis
	if s.MaxSteps != 20 {
		t.Errorf("MaxSteps = %d, want 20", s.MaxSteps)
	}
	if s.MaxFanout != 6 {
		t.Errorf("MaxFanout = %d, want 6", s.MaxFanout)
	}
	if s.MaxDepth != 3 {
		t.Errorf("MaxDepth = %d, want 3", s.MaxDepth)
	}
	if s.MaxAttempts != 2 {
		t.Errorf("MaxAttempts = %d, want 2", s.MaxAttempts)
	}
}

// TestLocalPipelineSynthesisTightenOnly asserts the ADR-0006 trust boundary for
// the repo-plantable .fuse.local.yml on the pipeline caps: it may LOWER a cap
// (tighten) but a value that would LOOSEN it (raise the cap) is ignored, keeping
// the trusted home-file value, and a warning is emitted for the loosened axis.
func TestLocalPipelineSynthesisTightenOnly(t *testing.T) {
	cwd := chdirTemp(t)
	home := filepath.Join(cwd, "home")
	if err := os.MkdirAll(filepath.Join(home, ".fuse"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	os.Unsetenv("LLM_GATEWAY_URL")
	os.Unsetenv("LLM_GATEWAY_KEY")

	// Trusted home file establishes the baseline caps.
	trusted := `
pipeline:
  synthesis:
    max_steps: 30
    max_fanout: 8
    max_depth: 4
    max_attempts: 3
`
	if err := os.WriteFile(filepath.Join(home, ".fuse", "config.yml"), []byte(trusted), 0o644); err != nil {
		t.Fatal(err)
	}
	// Untrusted local file tries to TIGHTEN max_steps (30 -> 10) and LOOSEN
	// max_fanout (8 -> 20).
	local := `
pipeline:
  synthesis:
    max_steps: 10
    max_fanout: 20
`
	if err := os.WriteFile(filepath.Join(cwd, ".fuse.local.yml"), []byte(local), 0o644); err != nil {
		t.Fatal(err)
	}

	warnings := captureWarnings(t)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	s := c.Pipeline.Synthesis
	if s.MaxSteps != 10 {
		t.Errorf("MaxSteps = %d, want 10 (local tighten honored)", s.MaxSteps)
	}
	if s.MaxFanout != 8 {
		t.Errorf("MaxFanout = %d, want 8 (local loosening ignored)", s.MaxFanout)
	}
	if w := warnings(); !strings.Contains(w, "pipeline.synthesis.max_fanout") {
		t.Errorf("expected a warning naming the loosened pipeline cap; got %q", w)
	}
}

// TestTightenNumeric pins the extracted ADR-0006 tighten-only helper directly,
// covering every branch for both int and float64 axes. This is the single
// source of truth the throughput/pipeline/queue_bound/tool_timeout merges now
// share, so its behavior must not drift.
func TestTightenNumeric(t *testing.T) {
	t.Run("int", func(t *testing.T) {
		cases := []struct {
			desc         string
			cur, newV    int
			trusted      bool
			wantCur      int
			wantReported bool
		}{
			{"omitted (0) is a no-op", 100, 0, false, 100, false},
			{"negative dropped even when trusted", 100, -5, true, 100, false},
			{"trusted sets any positive", 100, 500, true, 500, false},
			{"trusted may introduce from unset", 0, 500, true, 500, false},
			{"untrusted strict tighten honored", 100, 40, false, 40, false},
			{"untrusted raise is loosening", 100, 500, false, 100, true},
			{"untrusted equal is loosening (not strict)", 100, 100, false, 100, true},
			{"untrusted set-from-unset is loosening", 0, 40, false, 0, true},
		}
		for _, tc := range cases {
			t.Run(tc.desc, func(t *testing.T) {
				cur := tc.cur
				reported := tightenNumeric(&cur, tc.newV, tc.trusted)
				if cur != tc.wantCur {
					t.Errorf("cur = %d, want %d", cur, tc.wantCur)
				}
				if reported != tc.wantReported {
					t.Errorf("reported = %v, want %v", reported, tc.wantReported)
				}
			})
		}
	})

	t.Run("float64 (queue_bound)", func(t *testing.T) {
		// Trusted override.
		cur := 2.0
		if tightenNumeric(&cur, 3.5, true) || cur != 3.5 {
			t.Errorf("trusted float override failed: cur=%v", cur)
		}
		// Untrusted strict tighten.
		cur = 2.0
		if tightenNumeric(&cur, 1.5, false) || cur != 1.5 {
			t.Errorf("untrusted float tighten failed: cur=%v", cur)
		}
		// Untrusted raise is loosening.
		cur = 2.0
		if !tightenNumeric(&cur, 4.0, false) || cur != 2.0 {
			t.Errorf("untrusted float raise should be reported and dropped: cur=%v", cur)
		}
	})
}
