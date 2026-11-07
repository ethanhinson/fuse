// Package config loads fuse configuration from ~/.fuse/config.yml and
// .fuse.local.yml, falling back to a built-in default registry when absent.
package config

// Gateway describes the LiteLLM gateway endpoint.
type Gateway struct {
	URL string `yaml:"url"`
	Key string `yaml:"key"`
	// RequestTimeout bounds ONE gateway attempt (connect through the full
	// streamed body) as a Go duration string ("5m", "20m"). Empty means no limit
	// on total time. Reasoning models can stream 30k–60k
	// thinking tokens before their first visible byte, so a slow response is not
	// a failure; a silent one ends through the adapter's idle timeout. Not a credential or permission surface, so it is
	// honored from .fuse.local.yml as well (the worst case is a longer wait).
	RequestTimeout string `yaml:"request_timeout"`
}

// ModelConfig is a single named model entry.
type ModelConfig struct {
	ID        string `yaml:"id"`
	MaxTokens int    `yaml:"max_tokens"`
	// MaxTokensCeiling is the highest output cap a truncated reply may raise
	// max_tokens to, since reasoning spends the same budget as the reply. 0 = four
	// times max_tokens; set it equal to max_tokens to make max_tokens a hard limit.
	MaxTokensCeiling int    `yaml:"max_tokens_ceiling"`
	ContextWindow    int    `yaml:"context_window"` // model context size in tokens; 0 = harness default
	Persona          string `yaml:"persona"`
	SystemPrefix     string `yaml:"system_prefix"` // prepended before persona prompt (e.g. "/no_think" for Qwen3)
	// ReasoningEffort is sent as reasoning_effort on this model's agent calls:
	// none, minimal, low, medium, high or xhigh. Empty sends nothing and leaves
	// the provider's default. Providers differ in how they honour it.
	ReasoningEffort string `yaml:"reasoning_effort,omitempty"`
	// Operator display metadata shown by the model listings (change 0086);
	// nothing else reads it. Well-known tags: tools, vision, reasoning,
	// long-context; other strings pass through.
	Label       string   `yaml:"label,omitempty"`
	Description string   `yaml:"description,omitempty"`
	Tags        []string `yaml:"tags,omitempty"`
	// Advertise false hides the entry from the model listings; nil means advertised.
	Advertise *bool `yaml:"advertise,omitempty"`
}

// ModelsConfig holds the default model alias and all named entries.
// Entries are captured separately because YAML mixes `default` with the
// model map at the same level.
type ModelsConfig struct {
	Default string
	Entries map[string]ModelConfig
}

// SkillsConfig selects where installed skills are loaded from. Dirs empty falls
// back to skills.DefaultDirs().
type SkillsConfig struct {
	Dirs []string `yaml:"dirs"`
}

// PermissionsConfig controls the HITL gate behaviour.
type PermissionsConfig struct {
	Mode         string     `yaml:"mode"`          // off | prompt-all | smart | auto (default: smart)
	SessionAllow bool       `yaml:"session_allow"` // whether [s]ession option appears
	AutoApprove  []string   `yaml:"auto_approve"`  // patterns promoted beyond the safe list
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
	// FetchDeny and FetchAsk are host-glob floors for the web_fetch auto-mode gate:
	// a matched host is denied (FetchDeny) or forced to a human ask (FetchAsk) before
	// the classifier is ever consulted. Unlike classifier_model/deny/ask (which are
	// LOOSENING and honored only from the trusted ~/.fuse/config.yml), these are
	// TIGHTENING keys (ADR-0006) and MAY come from the repo-plantable .fuse.local.yml:
	// a checked-in file can only add host denials/asks, never weaken the gate.
	FetchDeny []string `yaml:"fetch_deny"`
	FetchAsk  []string `yaml:"fetch_ask"`
	// AllowPush deterministically allows `git push` in auto mode (change 0068).
	// LOOSENING: honored only from the trusted ~/.fuse/config.yml. Without it,
	// push routes to the context-aware classifier (never a terminal deny).
	AllowPush bool `yaml:"allow_push"`
	// WriteRoots are extra directories treated as workspace-equivalent by the
	// auto-mode path scoping (e.g. /tmp) — mutations and edit-tool writes inside
	// any of them auto-approve. LOOSENING: trusted-source-only. Each root is
	// canonicalized (EvalSymlinks) at gate wiring, not here.
	WriteRoots []string `yaml:"write_roots"`
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
//
// Type selects the credential: none (or unset) sends none; bearer and static send
// ClientSecret as a bearer token; oauth2 runs the browser flow and caches a token.
type MCPAuthConfig struct {
	Type         string   `yaml:"type"`
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

// SummarizationConfig configures Tier 2 anchored LLM summarization (change
// 0027). Enabled defaults on; Model empty ⇒ the session's main model (D4);
// Threshold shares Tier 1's context-window fraction; MaxOutput caps the
// summarizer's output tokens.
type SummarizationConfig struct {
	Enabled   bool    `yaml:"enabled"`
	Model     string  `yaml:"model"`
	Threshold float64 `yaml:"threshold"`
	MaxOutput int     `yaml:"max_output"`
}

// RelevanceConfig configures relevance-aware tool-result pruning (change 0028).
// Heuristic defaults on; false ⇒ pure-recency (today's behavior). RecencyFloorPct
// is the fraction of the protection budget reserved for the guaranteed recency
// floor. BodyScanBytes caps the result-body prefix scanned for overlap + dep
// tokens. ClassifierModel empty ⇒ heuristic only; else the model id used for the
// borderline band. ClassifierBatchSize is candidates per classifier call.
// BorderlineLo/Hi bound the heuristic scores sent to the classifier.
type RelevanceConfig struct {
	Heuristic           bool    `yaml:"heuristic"`
	RecencyFloorPct     int     `yaml:"recency_floor_pct"`
	BodyScanBytes       int     `yaml:"body_scan_bytes"`
	ClassifierModel     string  `yaml:"classifier_model"`
	ClassifierBatchSize int     `yaml:"classifier_batch_size"`
	BorderlineLo        float64 `yaml:"borderline_lo"`
	BorderlineHi        float64 `yaml:"borderline_hi"`
}

// ContextConfig groups context-management knobs. It holds the summarization
// block (change 0027) and the relevance block (change 0028); context_window
// stays on ModelConfig.
type ContextConfig struct {
	Summarization SummarizationConfig `yaml:"summarization"`
	Relevance     RelevanceConfig     `yaml:"relevance"`
}

// ObservabilityConfig controls the optional trace export. The zero value keeps it
// inert and requires no collector.
type ObservabilityConfig struct {
	Traces     TracesObservabilityConfig `yaml:"traces"`
	InstanceID string                    `yaml:"instance_id"`
}

type TracesObservabilityConfig struct {
	Enabled       bool              `yaml:"enabled"`
	Endpoint      string            `yaml:"endpoint"`
	Protocol      string            `yaml:"protocol"` // grpc | http/protobuf
	Insecure      bool              `yaml:"insecure"`
	Headers       map[string]string `yaml:"headers"`
	QueueSize     int               `yaml:"queue_size"`
	BatchSize     int               `yaml:"batch_size"`
	ExportTimeout string            `yaml:"export_timeout"`
	BatchTimeout  string            `yaml:"batch_timeout"`
	SampleRatio   float64           `yaml:"sample_ratio"`
}

// MaxInstructionsChars caps the top-level `instructions` brief, counted in
// characters (runes), not bytes.
const MaxInstructionsChars = 16384

// MaxRoles caps the number of top-level roles.
const MaxRoles = 32

// MaxRoleNameChars and RoleNamePattern are the rules on a role's name (its
// key under roles).
const (
	MaxRoleNameChars = 64
	RoleNamePattern  = `^[a-z0-9][a-z0-9_-]*$`
)

// MaxRoleDescriptionChars caps a role description, counted in characters.
// It matches the max_len on Role.description.
const MaxRoleDescriptionChars = 512

// RoleConfig is a named, reusable helper definition: a description the main
// agent reads when choosing whom to hand work to, a brief for the helper's
// system prompt, and optionally a tool set (empty: inherit the parent's) and
// a model alias (empty: inherit the parent's). The main agent picks a role
// through spawn_agent's `role` parameter; the child is built from the role's
// brief, tools and model (change 0114).
type RoleConfig struct {
	Description  string   `yaml:"description"`
	Instructions string   `yaml:"instructions"`
	Tools        []string `yaml:"tools"`
	Model        string   `yaml:"model"`
	// NoTools builds the role's child with no tools at all. It conflicts with a non-empty Tools.
	NoTools bool `yaml:"no_tools"`
}

// Config is the fully resolved fuse configuration.
type Config struct {
	// Instructions is the main agent's task brief, composed after the persona
	// prompt. Empty leaves the system prompt unchanged. It is not a permission,
	// so it is honored from every layer, .fuse.local.yml included.
	Instructions string
	// NoTools composes the main agent with no tools at all (change 0116): no
	// platform, MCP or internal tools, no spawn. Honored from every layer: it
	// only removes tools.
	NoTools    bool
	Gateway    Gateway
	Models     ModelsConfig
	SkillPaths []string
	// Skills configures the directories the installed skill set comes from. When
	// Dirs is empty the call site falls back to skills.DefaultDirs().
	Skills SkillsConfig
	// MaxTurns is a pointer so an omitted `max_turns` (nil) is distinguishable
	// from an explicit `max_turns: 0`. nil = unset ⇒ the call site applies the
	// context-aware backstop (unlimited in the interactive shell, 100 headless);
	// a non-nil 0 = explicitly unlimited everywhere; a non-nil N>0 caps every
	// context. Same omitted-key discipline as Permissions.SessionAllow. (0038)
	MaxTurns *int
	// NoProgressCalls is how many tool calls a headless run may make without a
	// file change before the model is nudged. nil = the agent default; 0 = off.
	NoProgressCalls *int
	// FinishCheck asks a headless root agent once whether it is really done when it
	// first replies with no tool call. Off by default: it adds one model call per run.
	FinishCheck bool
	// MaxRunTokens caps the input and output tokens one run's whole agent tree may
	// use. 0 = no cap.
	MaxRunTokens int64
	MaxTokens    int
	Permissions  PermissionsConfig
	MCPServers   []MCPServerConfig
	Research     ResearchConfig
	Agents       AgentsConfig
	// Throughput is the turn-level rate/quota surface (change 0036): global
	// requests-per-minute / tokens-per-minute smoothing plus an optional session
	// token ceiling, with optional per-provider rate overrides. Every numeric is
	// 0 = unlimited/unset, so an absent `throughput:` block is byte-identical to
	// pre-0036 behavior (no gate, no quota). Parsed and carried here; the rate
	// gate (Task 5) and session-quota enforcement (Task 6) consume it.
	Throughput ThroughputConfig
	// Roles are named helper definitions keyed by role name. Honored from the
	// home file (an entry replaces any earlier entry with the same name); inert from .fuse.local.yml, since a role carries a tool set and
	// a model pin.
	Roles map[string]RoleConfig
	// Context groups context-management knobs (change 0027). Its summarization
	// block defaults to Tier 2 on with a 0.85 threshold and 2000-token output
	// cap; the summarizer stays inert until a Completer is wired at the call
	// site, so the defaults are behavior-identical to pre-0027 until then.
	Context       ContextConfig
	Observability ObservabilityConfig
}

// ThroughputConfig is the turn-level rate and quota surface (change 0036). Each
// numeric is 0 = unlimited/unset so an omitted `throughput:` block reproduces
// pre-0036 behavior exactly. RequestsPerMinute/TokensPerMinute smooth dispatch
// through the gateway (the rate gate at Adapter.Complete — Task 5);
// SessionTokens is a global lifetime token ceiling (Task 6). Providers holds
// optional per-provider rate overrides keyed by provider name (fuse fronts
// several providers through one gateway, and their limits differ).
type ThroughputConfig struct {
	RequestsPerMinute int                           `yaml:"requests_per_minute"`
	TokensPerMinute   int                           `yaml:"tokens_per_minute"`
	SessionTokens     int                           `yaml:"session_tokens"`
	Providers         map[string]ProviderThroughput `yaml:"providers"`
}

// ProviderThroughput is a per-provider rate override under
// throughput.providers.<name>. Only the rate axes are per-provider; the session
// token ceiling is global. 0 = unset (fall back to the global limit).
type ProviderThroughput struct {
	RequestsPerMinute int `yaml:"requests_per_minute"`
	TokensPerMinute   int `yaml:"tokens_per_minute"`
}

// AgentsConfig controls the subagent runtime. MaxSpawns is a tree-global
// budget: the total number of child agents a single root turn may create, ever,
// counted against the append-only AgentTree. It backstops runaway fan-out; the
// budget line injected into each spawn_agent result is what steers the model to
// stop before it is reached. MaxConcurrent is the semaphore bound on children
// running at once — it caps live concurrency (and doubles as the default strip
// cap) independently of the total MaxSpawns budget.
// QueueBound is the per-pool pending-queue multiplier (change 0036): a pool's
// FIFO holds at most ceil(QueueBound × pool slots) waiters. 0 = unset ⇒ the
// scheduler's built-in default (2.0) is applied where consumed, keeping the
// zero-value = unset idiom the rest of this package uses for defaulted numerics
// (the default is not baked into the parsed struct so the tighten-only local
// merge can distinguish "unset" from an explicit value).
type AgentsConfig struct {
	MaxSpawns     int     `yaml:"max_spawns"`
	MaxConcurrent int     `yaml:"max_concurrent"`
	QueueBound    float64 `yaml:"queue_bound"`
	// ToolTimeoutSeconds bounds a single leaf tool call (bash, read, web_fetch,
	// …). 0 selects the agent default (120s). spawn_agent is always exempt: it
	// awaits child agents by design.
	ToolTimeoutSeconds int `yaml:"tool_timeout_seconds"`
}

// rawConfig mirrors the on-disk YAML shape before normalization.
type rawConfig struct {
	Gateway         Gateway                `yaml:"gateway"`
	Models          map[string]interface{} `yaml:"models"`
	SkillPaths      []string               `yaml:"skill_paths"`
	Skills          SkillsConfig           `yaml:"skills"`
	MaxTurns        *int                   `yaml:"max_turns"`
	NoProgressCalls *int                   `yaml:"no_progress_calls"`
	MaxRunTokens    int64                  `yaml:"max_run_tokens"`
	FinishCheck     bool                   `yaml:"finish_check"`
	MaxTokens       int                    `yaml:"max_tokens"`
	Instructions    string                 `yaml:"instructions"`
	NoTools         bool                   `yaml:"no_tools"`
	Permissions     rawPermissionsConfig   `yaml:"permissions"`
	MCPServers      []MCPServerConfig      `yaml:"mcp_servers"`
	Research        rawResearchConfig      `yaml:"research"`
	Agents          rawAgentsConfig        `yaml:"agents"`
	// Throughput reuses the resolved ThroughputConfig shape on-disk (plain
	// ints/maps, no free-text scalars — yaml.Unmarshal is safe); the tighten-only
	// merge happens in mergeFile, not here.
	Throughput rawThroughputConfig      `yaml:"throughput"`
	Projects   map[string]ProjectConfig `yaml:"projects"`
	// Roles mirrors Config.Roles on-disk. Descriptions and instructions are free
	// text; yaml.Unmarshal reads quoted and block scalars exactly.
	Roles map[string]RoleConfig `yaml:"roles"`
	// Context mirrors ContextConfig on-disk (change 0027). Its summarization
	// mirror uses a *bool for enabled so an omitted key keeps the true default
	// while `enabled: false` takes effect (mirrors RespectRobots).
	Context       rawContextConfig       `yaml:"context"`
	Observability rawObservabilityConfig `yaml:"observability"`
}

// rawObservabilityConfig preserves YAML presence for trace sample_ratio so an
// explicit zero can disable root-span sampling while an omitted value receives
// the resolved default.
type rawObservabilityConfig struct {
	Traces     rawTracesObservabilityConfig `yaml:"traces"`
	InstanceID string                       `yaml:"instance_id"`
}

type rawTracesObservabilityConfig struct {
	Enabled       bool              `yaml:"enabled"`
	Endpoint      string            `yaml:"endpoint"`
	Protocol      string            `yaml:"protocol"`
	Insecure      bool              `yaml:"insecure"`
	Headers       map[string]string `yaml:"headers"`
	QueueSize     int               `yaml:"queue_size"`
	BatchSize     int               `yaml:"batch_size"`
	ExportTimeout string            `yaml:"export_timeout"`
	BatchTimeout  string            `yaml:"batch_timeout"`
	SampleRatio   *float64          `yaml:"sample_ratio"`
}

func (o rawObservabilityConfig) resolve() ObservabilityConfig {
	ratio := 1.0
	if o.Traces.SampleRatio != nil {
		ratio = *o.Traces.SampleRatio
	}
	return ObservabilityConfig{
		Traces: TracesObservabilityConfig{
			Enabled: o.Traces.Enabled, Endpoint: o.Traces.Endpoint, Protocol: o.Traces.Protocol,
			Insecure: o.Traces.Insecure, Headers: o.Traces.Headers, QueueSize: o.Traces.QueueSize,
			BatchSize: o.Traces.BatchSize, ExportTimeout: o.Traces.ExportTimeout,
			BatchTimeout: o.Traces.BatchTimeout, SampleRatio: ratio,
		},
		InstanceID: o.InstanceID,
	}
}

// rawContextConfig mirrors ContextConfig on-disk.
type rawContextConfig struct {
	Summarization rawSummarizationConfig `yaml:"summarization"`
	Relevance     rawRelevanceConfig     `yaml:"relevance"`
}

// rawRelevanceConfig mirrors RelevanceConfig on-disk. Heuristic is a *bool so
// YAML can distinguish an omitted key (keep the true default) from an explicit
// `heuristic: false`; the other fields are plain scalars (0/"" = unset).
type rawRelevanceConfig struct {
	Heuristic           *bool   `yaml:"heuristic"`
	RecencyFloorPct     int     `yaml:"recency_floor_pct"`
	BodyScanBytes       int     `yaml:"body_scan_bytes"`
	ClassifierModel     string  `yaml:"classifier_model"`
	ClassifierBatchSize int     `yaml:"classifier_batch_size"`
	BorderlineLo        float64 `yaml:"borderline_lo"`
	BorderlineHi        float64 `yaml:"borderline_hi"`
}

// rawSummarizationConfig mirrors SummarizationConfig on-disk. Enabled is a
// *bool so YAML can distinguish an omitted key (keep the true default) from an
// explicit `enabled: false`; the other fields are plain scalars (0/"" = unset).
type rawSummarizationConfig struct {
	Enabled   *bool   `yaml:"enabled"`
	Model     string  `yaml:"model"`
	Threshold float64 `yaml:"threshold"`
	MaxOutput int     `yaml:"max_output"`
}

// ProjectConfig is a single per-project override entry keyed by absolute
// project path in the `projects:` map. It reuses rawPermissionsConfig (not the
// resolved PermissionsConfig) so the same session_allow *bool omitted-key
// discipline and the same trusted-merge path apply to a project entry. A
// matching entry resolves INTO c.Permissions at load time; there is no resolved
// Config.Projects surface.
type ProjectConfig struct {
	Permissions rawPermissionsConfig `yaml:"permissions"`
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
	MaxSpawns          int     `yaml:"max_spawns"`
	MaxConcurrent      int     `yaml:"max_concurrent"`
	QueueBound         float64 `yaml:"queue_bound"`
	ToolTimeoutSeconds int     `yaml:"tool_timeout_seconds"`
}

// rawThroughputConfig mirrors ThroughputConfig on-disk. Every numeric is
// 0 = unset, so a plain-int mirror suffices (no pointer-vs-zero ambiguity as
// with pointers elsewhere): a present value is nonzero. The tighten-only local
// merge (change 0036, ADR-0006) reads these in mergeFile.
type rawThroughputConfig struct {
	RequestsPerMinute int                           `yaml:"requests_per_minute"`
	TokensPerMinute   int                           `yaml:"tokens_per_minute"`
	SessionTokens     int                           `yaml:"session_tokens"`
	Providers         map[string]ProviderThroughput `yaml:"providers"`
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
		Observability: ObservabilityConfig{Traces: TracesObservabilityConfig{SampleRatio: 1}},
		Gateway:       Gateway{URL: "http://localhost:4000/v1", Key: "llm-gateway-local"},
		Models:        ModelsConfig{Default: "deepseek-flash", Entries: map[string]ModelConfig{}},
		// MaxTurns intentionally left nil (unset): the context-aware backstop is
		// applied at the call site in cmd/fuse (unlimited shell / 100 headless),
		// not baked into the config default. See change 0038.
		//
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
			MaxSpawns:     16,
			MaxConcurrent: 8,
		},
		// Tier 2 anchored summarization defaults on (change 0027) with the
		// documented threshold/output cap. Model empty ⇒ the main model (D4).
		// The summarizer stays inert until a Completer is wired at the call
		// site, so this default is behavior-identical to pre-0027.
		Context: ContextConfig{
			Summarization: SummarizationConfig{
				Enabled:   true,
				Threshold: 0.85,
				MaxOutput: 8000,
			},
			// Relevance-aware pruning defaults on (change 0028). Heuristic-only
			// (ClassifierModel empty); the recency floor keeps the newest half of
			// the protection budget, byte-identical to pre-0028 for that half.
			Relevance: RelevanceConfig{
				Heuristic:           true,
				RecencyFloorPct:     50,
				BodyScanBytes:       2048,
				ClassifierBatchSize: 10,
				BorderlineLo:        0.30,
				BorderlineHi:        0.60,
			},
		},
	}
}
