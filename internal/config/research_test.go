package config

import (
	"os"
	"path/filepath"
	"testing"
)

// writeHomeConfig writes cfg to $HOME/.fuse/config.yml under a fresh temp home
// and returns the resolved Config from Load(), with env overrides cleared.
func writeHomeConfig(t *testing.T, cfg string) Config {
	t.Helper()
	home := t.TempDir()
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

func TestDefaultResearch(t *testing.T) {
	r := Default().Research
	if r.MaxQueries != 5 {
		t.Errorf("MaxQueries = %d, want 5", r.MaxQueries)
	}
	if r.MaxResults != 5 {
		t.Errorf("MaxResults = %d, want 5", r.MaxResults)
	}
	if r.MaxContentKB != 50 {
		t.Errorf("MaxContentKB = %d, want 50", r.MaxContentKB)
	}
	if !r.RespectRobots {
		t.Error("RespectRobots = false, want true")
	}
	if r.Provider != "" {
		t.Errorf("Provider = %q, want empty", r.Provider)
	}
	if r.Custom.URL != "" {
		t.Errorf("Custom.URL = %q, want empty", r.Custom.URL)
	}
	if r.Custom.ResultsPath != "results" {
		t.Errorf("Custom.ResultsPath = %q, want results", r.Custom.ResultsPath)
	}
	if r.Custom.TitleField != "title" {
		t.Errorf("Custom.TitleField = %q, want title", r.Custom.TitleField)
	}
	if r.Custom.URLField != "url" {
		t.Errorf("Custom.URLField = %q, want url", r.Custom.URLField)
	}
	if r.Custom.SnippetField != "content" {
		t.Errorf("Custom.SnippetField = %q, want content", r.Custom.SnippetField)
	}
}

func TestResearchBlockOverridesDefaults(t *testing.T) {
	c := writeHomeConfig(t, `
research:
  provider: brave
  max_results: 8
`)
	if c.Research.Provider != "brave" {
		t.Errorf("Provider = %q, want brave", c.Research.Provider)
	}
	if c.Research.MaxResults != 8 {
		t.Errorf("MaxResults = %d, want 8", c.Research.MaxResults)
	}
	// Unspecified fields keep their defaults.
	if c.Research.MaxQueries != 5 {
		t.Errorf("MaxQueries = %d, want 5", c.Research.MaxQueries)
	}
	if c.Research.MaxContentKB != 50 {
		t.Errorf("MaxContentKB = %d, want 50", c.Research.MaxContentKB)
	}
	if !c.Research.RespectRobots {
		t.Error("RespectRobots = false, want true")
	}
}

func TestResearchCustomPartialBlockKeepsDefaults(t *testing.T) {
	c := writeHomeConfig(t, `
research:
  provider: custom
  custom:
    url: http://localhost:8888/search
    snippet_field: body
`)
	if c.Research.Provider != "custom" {
		t.Errorf("Provider = %q, want custom", c.Research.Provider)
	}
	if c.Research.Custom.URL != "http://localhost:8888/search" {
		t.Errorf("Custom.URL = %q", c.Research.Custom.URL)
	}
	if c.Research.Custom.SnippetField != "body" {
		t.Errorf("Custom.SnippetField = %q, want body", c.Research.Custom.SnippetField)
	}
	// Unset custom fields keep SearXNG defaults.
	if c.Research.Custom.ResultsPath != "results" {
		t.Errorf("Custom.ResultsPath = %q, want results", c.Research.Custom.ResultsPath)
	}
	if c.Research.Custom.TitleField != "title" {
		t.Errorf("Custom.TitleField = %q, want title", c.Research.Custom.TitleField)
	}
	if c.Research.Custom.URLField != "url" {
		t.Errorf("Custom.URLField = %q, want url", c.Research.Custom.URLField)
	}
}

func TestResearchRespectRobotsFalse(t *testing.T) {
	c := writeHomeConfig(t, `
research:
  respect_robots: false
`)
	if c.Research.RespectRobots {
		t.Error("RespectRobots = true, want false")
	}
}
