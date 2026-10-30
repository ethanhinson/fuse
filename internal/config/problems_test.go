package config

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// TestValidateMessagesUnchanged pins the exact error text Validate returns for
// each rule. Change 0084 moved the rules behind Problems(); the CLI's startup
// message must stay byte-identical.
func TestValidateMessagesUnchanged(t *testing.T) {
	cases := []struct {
		mutate func(*Config)
		want   string
	}{
		{func(c *Config) { c.Permissions.Mode = "smrat" }, `config: permissions.mode "smrat" is invalid (want off, prompt-all, smart, or auto)`},
		{func(c *Config) { c.MaxTokens = -1 }, "config: max_tokens must be >= 0, got -1"},
		{func(c *Config) { n := -3; c.MaxTurns = &n }, "config: max_turns must be >= 0 (0 = unlimited), got -3"},
		{func(c *Config) { c.Context.Summarization.Threshold = 1.5 }, "config: context.summarization.threshold must be in [0,1], got 1.5"},
		{func(c *Config) { c.Context.Relevance.BorderlineLo = 0.9; c.Context.Relevance.BorderlineHi = 0.2 }, "config: context.relevance borderline band [0.9,0.2] is invalid (want 0<=lo<=hi<=1)"},
		{func(c *Config) { c.Context.Relevance.RecencyFloorPct = 150 }, "config: context.relevance.recency_floor_pct must be in [0,100], got 150"},
		{func(c *Config) { c.LoopServer.LeaseTTL = "-5s" }, "config: loop_server.lease_ttl must be >= 0, got -5s"},
	}
	for _, tc := range cases {
		cfg := Default()
		tc.mutate(&cfg)
		err := cfg.Validate()
		if err == nil || err.Error() != tc.want {
			t.Errorf("Validate() = %v, want %q", err, tc.want)
		}
	}
}

// TestValidateLeaseTTLKeepsWrappedParseError guards that the duration parse
// error stays wrapped (errors.As reaches it) after the Problems refactor.
func TestValidateLeaseTTLKeepsWrappedParseError(t *testing.T) {
	cfg := Default()
	cfg.LoopServer.LeaseTTL = "30"
	err := cfg.Validate()
	if err == nil || !strings.HasPrefix(err.Error(), `config: loop_server.lease_ttl "30" is not a valid duration`) {
		t.Fatalf("Validate() = %v", err)
	}
	if _, perr := time.ParseDuration("30"); perr == nil || !strings.Contains(err.Error(), perr.Error()) {
		t.Fatalf("wrapped parse error missing from %v", err)
	}
	if errors.Unwrap(err) == nil {
		t.Fatalf("Validate() error no longer wraps the parse error")
	}
}

// TestProblemsCollectsEveryRuleWithPaths proves Problems reports every failing
// rule (not just the first) and that each carries the dotted YAML path a caller
// can place the error against.
func TestProblemsCollectsEveryRuleWithPaths(t *testing.T) {
	cfg := Default()
	cfg.Permissions.Mode = "nope"
	cfg.MaxTokens = -1
	cfg.Context.Summarization.Threshold = 2
	cfg.Context.Relevance.BorderlineLo = 0.9
	cfg.Context.Relevance.BorderlineHi = 0.2
	cfg.Context.Relevance.RecencyFloorPct = 101

	got := map[string]string{}
	for _, p := range cfg.Problems() {
		got[p.Path] = p.Message
	}
	want := map[string]string{
		"permissions.mode":                    `"nope" is invalid (want off, prompt-all, smart, or auto)`,
		"max_tokens":                          "must be >= 0, got -1",
		"context.summarization.threshold":     "must be in [0,1], got 2",
		"context.relevance.borderline_lo":     "borderline band [0.9,0.2] is invalid (want 0<=lo<=hi<=1)",
		"context.relevance.recency_floor_pct": "must be in [0,100], got 101",
	}
	if len(got) != len(want) {
		t.Fatalf("Problems() paths = %v, want %v", got, want)
	}
	for path, msg := range want {
		if got[path] != msg {
			t.Errorf("Problems()[%s] = %q, want %q", path, got[path], msg)
		}
	}
}

// TestProblemsBorderlineHiPath: an out-of-range upper bound is attributed to
// borderline_hi, not borderline_lo.
func TestProblemsBorderlineHiPath(t *testing.T) {
	cfg := Default()
	cfg.Context.Relevance.BorderlineHi = 1.5
	ps := cfg.Problems()
	if len(ps) != 1 || ps[0].Path != "context.relevance.borderline_hi" {
		t.Fatalf("Problems() = %+v", ps)
	}
}

func TestProblemsDefaultIsEmpty(t *testing.T) {
	if ps := Default().Problems(); len(ps) != 0 {
		t.Fatalf("Default().Problems() = %+v", ps)
	}
}

// TestFromYAMLMatchesTrustedLoad: FromYAML is the loader's own trusted merge
// over Default(), so a document resolves exactly as it would from the trusted
// home file.
func TestFromYAMLMatchesTrustedLoad(t *testing.T) {
	doc := []byte(`
max_tokens: 2048
models:
  default: fast
  fast:
    id: cloud/x
    max_tokens: 1000
permissions:
  mode: auto
  session_allow: false
context:
  summarization:
    enabled: false
workflows:
  research:
    pool:
      total: 3
`)
	cfg, err := FromYAML(doc, "test doc")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxTokens != 2048 || cfg.Models.Default != "fast" || cfg.Models.Entries["fast"].ID != "cloud/x" {
		t.Fatalf("models/max_tokens not merged: %+v", cfg.Models)
	}
	if cfg.Permissions.Mode != "auto" || cfg.Permissions.SessionAllow {
		t.Fatalf("permissions not merged as trusted: %+v", cfg.Permissions)
	}
	if cfg.Context.Summarization.Enabled {
		t.Fatalf("explicit enabled:false lost")
	}
	// Per-field workflow merge onto the built-in research workflow.
	if w := cfg.Workflows["research"]; w.Pool.Total != 3 || w.Pool.Concurrent != 5 || w.Skill != "research" {
		t.Fatalf("workflow merge = %+v", w)
	}
	// Defaults survive for untouched keys.
	if cfg.Agents.MaxSpawns != 64 || cfg.Context.Summarization.Threshold != 0.85 {
		t.Fatalf("defaults lost: %+v %+v", cfg.Agents, cfg.Context.Summarization)
	}
}

func TestFromYAMLParseErrorNamesSource(t *testing.T) {
	_, err := FromYAML([]byte("max_tokens: [nope"), "harness spec")
	if err == nil || !strings.Contains(err.Error(), "harness spec") {
		t.Fatalf("FromYAML error = %v", err)
	}
}
