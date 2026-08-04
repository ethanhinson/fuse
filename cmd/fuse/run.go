package main

import (
	"fmt"
	"io"
	"net/http"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/tools"
	"github.com/ethanhinson/fuse/internal/tui"
)

// registryFromConfig builds a model.Registry, starting from the built-in
// default and overlaying any config-defined entries.
func registryFromConfig(cfg config.Config) *model.Registry {
	reg := model.DefaultRegistry()
	if cfg.Models.Default != "" {
		reg.Default = cfg.Models.Default
	}
	for alias, mc := range cfg.Models.Entries {
		reg = mergeEntry(reg, alias, model.ModelConfig{ID: mc.ID, MaxTokens: mc.MaxTokens, Persona: mc.Persona, SystemPrefix: mc.SystemPrefix})
	}
	return reg
}

// mergeEntry returns a registry with alias set to mc, preserving other entries.
func mergeEntry(reg *model.Registry, alias string, mc model.ModelConfig) *model.Registry {
	entries := map[string]model.ModelConfig{}
	for _, name := range reg.Names() {
		v, _ := reg.Resolve(name)
		entries[name] = v
	}
	entries[alias] = mc
	return model.NewRegistry(reg.Default, entries)
}

// defaultToolRegistry builds the full Phase 1 tool registry.
func defaultToolRegistry() *tools.Registry {
	r := tools.NewRegistry()
	for _, t := range tools.DefaultTools() {
		r.Register(t)
	}
	for _, t := range tools.CodeindexTools() {
		r.Register(t)
	}
	return r
}

// buildAgent resolves a model alias and constructs a ready-to-run Agent along
// with the resolved gateway model id. The persona system prompt is always
// prepended; extra is appended (e.g. a skill listing block from shell mode).
func buildAgent(cfg config.Config, reg *model.Registry, alias string, out io.Writer, verbose bool, extra string, traceW io.Writer) (*agent.Agent, string, error) {
	if alias == "" {
		alias = reg.Default
	}
	mc, err := reg.Resolve(alias)
	if err != nil {
		return nil, "", fmt.Errorf("model %q: %w", alias, err)
	}
	adapter := model.NewAdapter(cfg.Gateway.URL, cfg.Gateway.Key, http.DefaultClient)
	if traceW != nil {
		adapter = adapter.WithTrace(traceW)
	}
	toolReg := defaultToolRegistry()
	renderer := tui.NewRenderer(out, verbose)
	maxTokens := mc.MaxTokens
	if maxTokens == 0 {
		maxTokens = cfg.MaxTokens
	}
	systemPrompt := agent.ComposeSystemPrompt(mc.Persona, mc.SystemPrefix, extra)
	a := agent.New(adapter, toolReg, renderer, mc.ID, systemPrompt, cfg.MaxTurns, maxTokens)
	return a, mc.ID, nil
}
