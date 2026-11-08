package configcheck

import (
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
)

// The ceiling must survive config -> registry, or the setting silently does nothing.
func TestRegistryCarriesMaxTokensCeiling(t *testing.T) {
	cfg := config.Config{}
	cfg.Models.Entries = map[string]config.ModelConfig{"m": {ID: "cloud/m", MaxTokens: 100, MaxTokensCeiling: 800}}
	mc, err := Registry(cfg).Resolve("m")
	if err != nil {
		t.Fatal(err)
	}
	if mc.MaxTokensCeiling != 800 {
		t.Errorf("MaxTokensCeiling = %d", mc.MaxTokensCeiling)
	}
}

func TestRegistryCarriesAttachments(t *testing.T) {
	on, off := true, false
	cfg := config.Config{}
	cfg.Models.Entries = map[string]config.ModelConfig{
		"on": {ID: "cloud/a", Attachments: &on}, "off": {ID: "cloud/claude-b", Attachments: &off}, "unset": {ID: "cloud/c"},
	}
	reg := Registry(cfg)
	for alias, want := range map[string]bool{"on": true, "off": false, "unset": false} {
		mc, _ := reg.Resolve(alias)
		if got := mc.AcceptsAttachments(); got != want {
			t.Errorf("%s: AcceptsAttachments = %v, want %v", alias, got, want)
		}
	}
	if mc, _ := reg.Resolve("unset"); mc.Attachments != nil {
		t.Error("unset became non-nil")
	}
}
