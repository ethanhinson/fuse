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
