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
