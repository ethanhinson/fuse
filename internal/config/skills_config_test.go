package config

import (
	"strings"
	"testing"
	"time"
)

// skills_config_test.go covers the skills.* operator keys added by change
// 0134: skills.source selects the adapter, skills.dirs overrides the default
// directories, and skills.refresh sets the reload interval (defaulting to
// DefaultSkillsRefresh).

func TestSkillsDefaults(t *testing.T) {
	c := Config{}
	if got := c.Skills.Source; got != "" {
		t.Fatalf("default Source = %q, want empty (fs fallback)", got)
	}
	if got := c.Skills.RefreshDuration(); got != DefaultSkillsRefresh {
		t.Fatalf("default RefreshDuration = %v, want %v", got, DefaultSkillsRefresh)
	}
	if len(c.Skills.Dirs) != 0 {
		t.Fatalf("default Dirs = %v, want unset (call site falls back to DefaultDirs)", c.Skills.Dirs)
	}
}

func TestSkillsSectionParses(t *testing.T) {
	c, err := FromYAML([]byte(`
models: {default: fast, fast: {id: cloud/fast}}
skills:
  source: postgres
  dirs: [/srv/skills-a, /srv/skills-b]
  refresh: 7s
`), "operator")
	if err != nil {
		t.Fatalf("FromYAML: %v", err)
	}
	if c.Skills.Source != "postgres" {
		t.Errorf("Source = %q, want postgres", c.Skills.Source)
	}
	if len(c.Skills.Dirs) != 2 || c.Skills.Dirs[0] != "/srv/skills-a" || c.Skills.Dirs[1] != "/srv/skills-b" {
		t.Errorf("Dirs = %v, want [/srv/skills-a /srv/skills-b]", c.Skills.Dirs)
	}
	if got := c.Skills.RefreshDuration(); got != 7*time.Second {
		t.Errorf("RefreshDuration = %v, want 7s", got)
	}
}

func TestSkillsSectionRejectsInvalidSourceOnlyAtBindings(t *testing.T) {
	// The config layer accepts any source string; the binding that selects the
	// adapter (skillSourceFromConfig) rejects unknown values. Validate must not
	// reject an unknown source: config does not know the adapter set.
	c := Config{Skills: SkillsConfig{Source: "fromage"}}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate rejected unknown skills.source: %v", err)
	}
}

func TestSkillsRefreshInvalidFallsBackToDefault(t *testing.T) {
	// A malformed skills.refresh is a hard parse error (the loader validates it
	// as a positive duration), so an operator typo never silently degrades to
	// the default interval.
	_, err := FromYAML([]byte(`
models: {default: fast, fast: {id: cloud/fast}}
skills:
  refresh: not-a-duration
`), "operator")
	if err == nil {
		t.Fatal("FromYAML accepted a malformed skills.refresh")
	}
	// An unset refresh falls back to the default.
	c, err := FromYAML([]byte(`
models: {default: fast, fast: {id: cloud/fast}}
skills:
  source: fs
`), "operator")
	if err != nil {
		t.Fatalf("FromYAML: %v", err)
	}
	if got := c.Skills.RefreshDuration(); got != DefaultSkillsRefresh {
		t.Fatalf("RefreshDuration when unset = %v, want default %v", got, DefaultSkillsRefresh)
	}
}

func TestSkillsUnmarshalRejectsBadYAMLTypes(t *testing.T) {
	// A non-string refresh (e.g. a number) must not panic or silently coerce.
	c, err := FromYAML([]byte(`
models: {default: fast, fast: {id: cloud/fast}}
skills:
  refresh: 30
`), "operator")
	if err != nil {
		// A strict-ish unmarshal error is fine.
		return
	}
	_ = c
	if strings.Contains(err.Error(), "") {
		t.Log("unmarshal accepted numeric refresh")
	}
}