// Package config loads fuse configuration from ~/.fuse/config.yml and
// .fuse.local.yml, falling back to a built-in default registry when absent.
package config

// Gateway describes the LiteLLM gateway endpoint.
type Gateway struct {
	URL string `yaml:"url"`
	Key string `yaml:"key"`
}

// ModelConfig is a single named model entry.
type ModelConfig struct {
	ID        string `yaml:"id"`
	MaxTokens int    `yaml:"max_tokens"`
	Persona   string `yaml:"persona"`
}

// ModelsConfig holds the default model alias and all named entries.
// Entries are captured separately because YAML mixes `default` with the
// model map at the same level.
type ModelsConfig struct {
	Default string
	Entries map[string]ModelConfig
}

// Config is the fully resolved fuse configuration.
type Config struct {
	Gateway    Gateway
	Models     ModelsConfig
	SkillPaths []string
	MaxTurns   int
	MaxTokens  int
}

// rawConfig mirrors the on-disk YAML shape before normalization.
type rawConfig struct {
	Gateway    Gateway                `yaml:"gateway"`
	Models     map[string]interface{} `yaml:"models"`
	SkillPaths []string               `yaml:"skill_paths"`
	MaxTurns   int                    `yaml:"max_turns"`
	MaxTokens  int                    `yaml:"max_tokens"`
}

// Default returns the zero-config built-in configuration.
func Default() Config {
	return Config{
		Gateway:   Gateway{URL: "http://localhost:4000/v1", Key: "llm-gateway-local"},
		Models:    ModelsConfig{Default: "deepseek-flash", Entries: map[string]ModelConfig{}},
		MaxTurns:  25,
		MaxTokens: 8192,
	}
}
