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
	ID            string `yaml:"id"`
	MaxTokens     int    `yaml:"max_tokens"`
	ContextWindow int    `yaml:"context_window"` // model context size in tokens; 0 = harness default
	Persona       string `yaml:"persona"`
	SystemPrefix  string `yaml:"system_prefix"` // prepended before persona prompt (e.g. "/no_think" for Qwen3)
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
	Mode         string   `yaml:"mode"`          // off | prompt-all | smart | auto (default: smart)
	SessionAllow bool     `yaml:"session_allow"` // whether [s]ession option appears
	AutoApprove  []string `yaml:"auto_approve"`  // patterns promoted beyond the safe list
	AlwaysPrompt []string   `yaml:"always_prompt"` // patterns demoted to always-prompt
	Disabled     []string   `yaml:"disabled"`      // tool names fully disabled (Enabled: false)
	Auto         AutoConfig `yaml:"auto"`          // auto-mode classifier + static rule surface
}

// AutoConfig configures auto mode: the classifier model alias and the static
// deny/ask pattern lists layered around it. In auto mode a bash command is
// split into per-segment simple commands and each is run through a layered
// pipeline — static rules (Deny/Ask plus the built-in dangerous set) win first,
// then the read-only safe list auto-approves, then path/egress heuristics, and
// only genuinely gray-area segments reach the classifier model. Deny always
// beats Ask always beats allow, and an unparseable command fails closed.
type AutoConfig struct {
	ClassifierModel string   `yaml:"classifier_model"`
	Deny            []string `yaml:"deny"`
	Ask             []string `yaml:"ask"`
}

// CustomProviderConfig describes a user-supplied JSON search endpoint,
// shaped by default for a SearXNG `/search?format=json` response.
type CustomProviderConfig struct {
	URL          string            `yaml:"url"`
	Headers      map[string]string `yaml:"headers"`
	ResultsPath  string            `yaml:"results_path"`
	TitleField   string            `yaml:"title_field"`
	URLField     string            `yaml:"url_field"`
	SnippetField string            `yaml:"snippet_field"`
}

// ResearchConfig controls the research mode: which search provider to use and
// the crawl/extraction limits applied when gathering sources.
type ResearchConfig struct {
	Provider      string               `yaml:"provider"` // brave | tavily | custom | "" (auto)
	MaxQueries    int                  `yaml:"max_queries"`
	MaxResults    int                  `yaml:"max_results"`
	MaxContentKB  int                  `yaml:"max_content_kb"`
	RespectRobots bool                 `yaml:"respect_robots"`
	Custom        CustomProviderConfig `yaml:"custom"`
}

// MCPAuthConfig holds authentication settings for an HTTP MCP server.
type MCPAuthConfig struct {
	Type         string   `yaml:"type"`          // none | bearer | oauth2
	ClientID     string   `yaml:"client_id"`     // optional; absent = dynamic registration
	ClientSecret string   `yaml:"client_secret"` // optional; used as bearer token for type=bearer
	Scopes       []string `yaml:"scopes"`
	TokenFile    string   `yaml:"token_file"` // optional; default ~/.fuse/mcp-tokens/<name>.json
}

// MCPServerConfig describes a single MCP server to spawn.
type MCPServerConfig struct {
	Name      string            `yaml:"name"`
	Transport string            `yaml:"transport"` // stdio | http
	Command   []string          `yaml:"command"`   // stdio only
	URL       string            `yaml:"url"`       // http only
	Env       map[string]string `yaml:"env"`
	Auth      MCPAuthConfig     `yaml:"auth"` // http only
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
	Research    ResearchConfig
	Agents      AgentsConfig
}

// AgentsConfig controls the subagent runtime. MaxSpawns is a tree-global
// budget: the total number of child agents a single root turn may create, ever,
// counted against the append-only AgentTree. It backstops runaway fan-out; the
// budget line injected into each spawn_agent result is what steers the model to
// stop before it is reached.
type AgentsConfig struct {
	MaxSpawns int `yaml:"max_spawns"`
}

// rawConfig mirrors the on-disk YAML shape before normalization.
type rawConfig struct {
	Gateway     Gateway                `yaml:"gateway"`
	Models      map[string]interface{} `yaml:"models"`
	SkillPaths  []string               `yaml:"skill_paths"`
	MaxTurns    int                    `yaml:"max_turns"`
	MaxTokens   int                    `yaml:"max_tokens"`
	Permissions rawPermissionsConfig   `yaml:"permissions"`
	MCPServers  []MCPServerConfig      `yaml:"mcp_servers"`
	Research    rawResearchConfig      `yaml:"research"`
	Agents      rawAgentsConfig        `yaml:"agents"`
}

// rawPermissionsConfig mirrors PermissionsConfig on-disk. SessionAllow is a
// pointer so the loader can distinguish an omitted key from an explicit
// `session_allow: false`; a plain bool zero-value cannot, and the distinction
// matters for the trust-boundary check on the repo-plantable .fuse.local.yml.
type rawPermissionsConfig struct {
	Mode         string     `yaml:"mode"`
	SessionAllow *bool      `yaml:"session_allow"`
	AutoApprove  []string   `yaml:"auto_approve"`
	AlwaysPrompt []string   `yaml:"always_prompt"`
	Disabled     []string   `yaml:"disabled"`
	Auto         AutoConfig `yaml:"auto"`
}

// rawAgentsConfig mirrors AgentsConfig on-disk.
type rawAgentsConfig struct {
	MaxSpawns int `yaml:"max_spawns"`
}

// rawResearchConfig mirrors ResearchConfig on-disk. RespectRobots is a pointer
// so YAML can distinguish an omitted key (keep the true default) from an
// explicit `respect_robots: false`; a plain bool zero-value cannot.
type rawResearchConfig struct {
	Provider      string               `yaml:"provider"`
	MaxQueries    int                  `yaml:"max_queries"`
	MaxResults    int                  `yaml:"max_results"`
	MaxContentKB  int                  `yaml:"max_content_kb"`
	RespectRobots *bool                `yaml:"respect_robots"`
	Custom        CustomProviderConfig `yaml:"custom"`
}

// Default returns the zero-config built-in configuration.
func Default() Config {
	return Config{
		Gateway:   Gateway{URL: "http://localhost:4000/v1", Key: "llm-gateway-local"},
		Models:    ModelsConfig{Default: "deepseek-flash", Entries: map[string]ModelConfig{}},
		MaxTurns: 25,
		// Per-turn output ceiling. 16384 (up from 8192) so a full research
		// synthesis — report body plus its numbered source list — is not cut
		// mid-generation; still configurable per-model and via `max_tokens`.
		MaxTokens: 16384,
		Permissions: PermissionsConfig{
			Mode:         "smart",
			SessionAllow: true,
		},
		Research: ResearchConfig{
			MaxQueries:    5,
			MaxResults:    5,
			MaxContentKB:  50,
			RespectRobots: true,
			Custom: CustomProviderConfig{
				ResultsPath:  "results",
				TitleField:   "title",
				URLField:     "url",
				SnippetField: "content",
			},
		},
		Agents: AgentsConfig{
			MaxSpawns: 16,
		},
	}
}
