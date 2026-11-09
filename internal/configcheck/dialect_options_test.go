package configcheck

import (
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
)

func TestRegistryCarriesDialectOptions(t *testing.T) {
	on := true
	cfg := config.Default()
	cfg.Models.Entries = map[string]config.ModelConfig{
		"m": {ID: "cloud/m", DialectOptions: config.DialectOptions{CacheMarkers: &on, ReasoningField: "reasoning"}},
	}
	reg := Registry(cfg)
	on = false // must not reach the registry entry
	mc, err := reg.Resolve("m")
	if err != nil {
		t.Fatal(err)
	}
	if mc.DialectOptions.CacheMarkers == nil || !*mc.DialectOptions.CacheMarkers || mc.DialectOptions.ReasoningField != "reasoning" {
		t.Fatalf("options = %+v", mc.DialectOptions)
	}
}
