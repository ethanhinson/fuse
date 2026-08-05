package model

import (
	"fmt"
	"sort"
)

// ModelConfig is a resolved model entry inside the registry.
type ModelConfig struct {
	ID            string
	MaxTokens     int
	ContextWindow int // model context size in tokens; 0 = harness default (128k)
	Persona       string
	SystemPrefix  string // prepended before the persona system prompt; for model-specific directives
}

// Registry maps model aliases to their resolved gateway configuration.
type Registry struct {
	Default string
	entries map[string]ModelConfig
}

// NewRegistry builds a registry from an explicit default alias and entries.
func NewRegistry(def string, entries map[string]ModelConfig) *Registry {
	if entries == nil {
		entries = map[string]ModelConfig{}
	}
	return &Registry{Default: def, entries: entries}
}

// DefaultRegistry is the built-in zero-config model registry.
func DefaultRegistry() *Registry {
	return NewRegistry("deepseek-flash", map[string]ModelConfig{
		"deepseek-flash": {ID: "cloud/deepseek-v4-flash", MaxTokens: 8192, Persona: "coding"},
		"deepseek-pro":   {ID: "cloud/deepseek-v4-pro", MaxTokens: 8192, Persona: "coding"},
		"kimi":           {ID: "cloud/kimi-k3", MaxTokens: 8192, Persona: "research"},
		"glm":            {ID: "cloud/glm-5.2", MaxTokens: 8192, Persona: "general"},
		"qwen-cloud":     {ID: "cloud/qwen3-8b", MaxTokens: 4096, Persona: "general"},
		"qwen-coder":     {ID: "local/qwen-coder-7b", MaxTokens: 4096, Persona: "coding"},
		"qwen-local":     {ID: "local/qwen-7b", MaxTokens: 4096, Persona: "general"},
		"llama":          {ID: "local/llama3.1:8b", MaxTokens: 2048, Persona: "general"},
		"claude":         {ID: "claude/sonnet", MaxTokens: 8192, Persona: "general"},
		// Claude Sonnet 5 via the gateway's OpenRouter route (distinct from the
		// credit-blocked Anthropic "claude/sonnet" route above).
		"sonnet-5": {ID: "claude/sonnet-5", MaxTokens: 8192, Persona: "general"},
		// MiniMax M3, a cheap agentic gateway model.
		"minimax": {ID: "cloud/minimax-m3", MaxTokens: 8192, Persona: "general"},
		// claude-max routes to the CLIAdapter (fuse mcp-server bridge) rather
		// than the LiteLLM gateway. ID prefix "cli/" is the discriminator.
		"claude-max": {ID: "cli/claude-max", MaxTokens: 0, Persona: "coding"},
	})
}

// Resolve returns the config for an alias, or an error if unknown.
func (r *Registry) Resolve(alias string) (ModelConfig, error) {
	mc, ok := r.entries[alias]
	if !ok {
		return ModelConfig{}, fmt.Errorf("unknown model %q", alias)
	}
	return mc, nil
}

// Names returns all registered aliases, sorted.
func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.entries))
	for k := range r.entries {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}
