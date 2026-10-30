// Package configcheck holds the config validation rules that need more than the
// config itself - today, model-alias references, which need the resolved
// model.Registry (change 0084). It sits above internal/config (keeping config a
// leaf package) and below every caller that validates a config: the CLI startup
// gate in cmd/fuse and the harness-spec validator in
// github.com/ethanhinson/fuse/harness. Before change 0084 these rules lived in
// cmd/fuse (package main), where nothing else could reuse them.
package configcheck

import (
	"fmt"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/model"
)

// Registry builds a model.Registry, starting from the built-in default and
// overlaying any config-defined entries (a config alias that matches a built-in
// one replaces it).
func Registry(cfg config.Config) *model.Registry {
	reg := model.DefaultRegistry()
	if cfg.Models.Default != "" {
		reg.Default = cfg.Models.Default
	}
	for alias, mc := range cfg.Models.Entries {
		reg.Set(alias, model.ModelConfig{
			ID: mc.ID, MaxTokens: mc.MaxTokens, ContextWindow: mc.ContextWindow,
			Persona: mc.Persona, SystemPrefix: mc.SystemPrefix,
		})
	}
	return reg
}

// ModelRefProblems checks that every model alias referenced in cfg resolves in
// reg. A dangling reference otherwise fails only at the first gateway call for
// that model - often deep into a run - so surfacing it up front is a real
// usability win. The default model alias, both context-classifier models, the
// auto-mode classifier and the orchestrator model are checked; empty (unset)
// refs are skipped. Each problem's Err is the exact startup error the CLI has
// always printed.
func ModelRefProblems(cfg config.Config, reg *model.Registry) []config.Problem {
	var ps []config.Problem
	check := func(field, alias string) {
		if alias == "" {
			return
		}
		if _, err := reg.Resolve(alias); err != nil {
			ps = append(ps, config.Problem{
				Path:    field,
				Message: fmt.Sprintf("references unknown model %q", alias),
				Err:     fmt.Errorf("config: %s references unknown model %q; run `fuse models` to list configured aliases", field, alias),
			})
		}
	}
	check("models.default", cfg.Models.Default)
	check("context.summarization.model", cfg.Context.Summarization.Model)
	check("context.relevance.classifier_model", cfg.Context.Relevance.ClassifierModel)
	check("permissions.auto.classifier_model", cfg.Permissions.Auto.ClassifierModel)
	check("orchestrator.model", cfg.Orchestrator.Model)
	return ps
}

// Problems runs every config rule: the structural ones (config.Problems) then
// the model-reference ones against the registry cfg itself resolves.
func Problems(cfg config.Config) []config.Problem {
	ps := cfg.Problems()
	return append(ps, ModelRefProblems(cfg, Registry(cfg))...)
}
