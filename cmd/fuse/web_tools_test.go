package main

import (
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
)

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
