package main

import (
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
)

func TestValidateModelRefs(t *testing.T) {
	// registryFromConfig(Default) yields the built-in DefaultRegistry, whose
	// aliases are the baseline for these checks.
	base := config.Default()
	reg := registryFromConfig(base)

	t.Run("default config resolves", func(t *testing.T) {
		if err := validateModelRefs(base, reg); err != nil {
			t.Fatalf("default config should validate: %v", err)
		}
	})

	t.Run("unknown default model rejected", func(t *testing.T) {
		cfg := base
		cfg.Models.Default = "does-not-exist"
		if err := validateModelRefs(cfg, reg); err == nil {
			t.Fatal("expected error for unknown default model")
		}
	})

	t.Run("unknown summarization model rejected", func(t *testing.T) {
		cfg := base
		cfg.Context.Summarization.Model = "ghost-model"
		if err := validateModelRefs(cfg, reg); err == nil {
			t.Fatal("expected error for unknown summarization model")
		}
	})

	t.Run("unknown relevance classifier rejected", func(t *testing.T) {
		cfg := base
		cfg.Context.Relevance.ClassifierModel = "ghost-classifier"
		if err := validateModelRefs(cfg, reg); err == nil {
			t.Fatal("expected error for unknown relevance classifier model")
		}
	})

	t.Run("empty refs are skipped", func(t *testing.T) {
		cfg := base
		cfg.Context.Summarization.Model = ""
		cfg.Context.Relevance.ClassifierModel = ""
		cfg.Permissions.Auto.ClassifierModel = ""
		if err := validateModelRefs(cfg, reg); err != nil {
			t.Fatalf("empty refs should be skipped: %v", err)
		}
	})

	t.Run("a configured custom alias resolves", func(t *testing.T) {
		cfg := base
		cfg.Models.Entries = map[string]config.ModelConfig{
			"mymodel": {ID: "cloud/whatever"},
		}
		cfg.Context.Summarization.Model = "mymodel"
		reg2 := registryFromConfig(cfg)
		if err := validateModelRefs(cfg, reg2); err != nil {
			t.Fatalf("custom alias should resolve: %v", err)
		}
	})
}
