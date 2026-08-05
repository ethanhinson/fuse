package main

import (
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/skills"
	"github.com/ethanhinson/fuse/internal/tools"
)

// hasTool reports whether a registry exposes a tool by name.
func hasTool(reg *tools.Registry, name string) bool {
	for _, s := range reg.Schemas() {
		if s.Name == name {
			return true
		}
	}
	return false
}

// TestDefaultRegistrySkillToolFollowsLookup guards the one-shot fix: the skill
// tool is registered iff a non-nil skill lookup is supplied. A nil lookup (the
// old one-shot path) meant no skill tool at all, so `fuse "<task>"` could never
// invoke a skill.
func TestDefaultRegistrySkillToolFollowsLookup(t *testing.T) {
	cfg := config.Default()

	if hasTool(defaultToolRegistry(cfg.Research, nil), "skill") {
		t.Error("nil lookup must NOT register the skill tool")
	}

	set, err := skills.LoadWithEmbedded(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !hasTool(defaultToolRegistry(cfg.Research, set.Lookup), "skill") {
		t.Error("non-nil lookup MUST register the skill tool (one-shot skill access)")
	}
}

// The default tool registry must register web_search and web_fetch, and a
// Subset selecting them (plus the force-included spawn_agent) must contain both.
func TestDefaultRegistryIncludesWebTools(t *testing.T) {
	cfg := config.Default()
	reg := defaultToolRegistry(cfg.Research, nil)

	for _, name := range []string{"web_search", "web_fetch"} {
		found := false
		for _, s := range reg.Schemas() {
			if s.Name == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("default registry missing %q", name)
		}
	}

	sub, unknown := reg.Subset([]string{"web_search", "web_fetch"})
	if len(unknown) != 0 {
		t.Fatalf("unexpected unknown tools: %v", unknown)
	}
	have := map[string]bool{}
	for _, s := range sub.Schemas() {
		have[s.Name] = true
	}
	for _, name := range []string{"web_search", "web_fetch"} {
		if !have[name] {
			t.Errorf("subset missing %q", name)
		}
	}
}
