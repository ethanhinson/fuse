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
	ID           string `yaml:"id"`
	MaxTokens    int    `yaml:"max_tokens"`
	Persona      string `yaml:"persona"`
	SystemPrefix string `yaml:"system_prefix"` // prepended before persona prompt (e.g. "/no_think" for Qwen3)
}

// ModelsConfig holds the default model alias and all named entries.
// Entries are captured separately because YAML mixes `default` with the
// model map at the same level.
type ModelsConfig struct {
	Default string
	Entries map[string]ModelConfig
}

// PermissionsConfig controls the HITL gate behaviour.
type PermissionsConfig struct {
	Mode         string   `yaml:"mode"`          // off | prompt-all | smart (default: smart)
	SessionAllow bool     `yaml:"session_allow"` // whether [s]ession option appears
	AutoApprove  []string `yaml:"auto_approve"`  // patterns promoted beyond the safe list
	AlwaysPrompt []string `yaml:"always_prompt"` // patterns demoted to always-prompt
	Disabled     []string `yaml:"disabled"`      // tool names fully disabled (Enabled: false)
}

// MCPServerConfig describes a single MCP server to spawn.
type MCPServerConfig struct {
	Name      string            `yaml:"name"`
	Transport string            `yaml:"transport"` // stdio only in phase 1
	Command   []string          `yaml:"command"`
	Env       map[string]string `yaml:"env"`
}

// Config is the fully resolved fuse configuration.
type Config struct {
	Gateway     Gateway
	Models      ModelsConfig
	SkillPaths  []string
	MaxTurns    int
	MaxTokens   int
	Permissions PermissionsConfig
	MCPServers  []MCPServerConfig
}

// rawConfig mirrors the on-disk YAML shape before normalization.
type rawConfig struct {
	Gateway     Gateway                `yaml:"gateway"`
	Models      map[string]interface{} `yaml:"models"`
	SkillPaths  []string               `yaml:"skill_paths"`
	MaxTurns    int                    `yaml:"max_turns"`
	MaxTokens   int                    `yaml:"max_tokens"`
	Permissions PermissionsConfig      `yaml:"permissions"`
	MCPServers  []MCPServerConfig      `yaml:"mcp_servers"`
}

// Default returns the zero-config built-in configuration.
func Default() Config {
	return Config{
		Gateway:   Gateway{URL: "http://localhost:4000/v1", Key: "llm-gateway-local"},
		Models:    ModelsConfig{Default: "deepseek-flash", Entries: map[string]ModelConfig{}},
		MaxTurns:  25,
		MaxTokens: 8192,
		Permissions: PermissionsConfig{
			Mode:         "smart",
			SessionAllow: true,
		},
	}
}
