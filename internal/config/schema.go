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
// The `type` selects both the transport credential AND the identity-propagation
// tier (change #52). Identity-propagating types mint a per-call, audience-bound
// delegation token from the loop initiator's identity (TierOAuth); the legacy
// types carry a per-server static credential with NO initiator identity
// (TierStatic). Existing "bearer"/"oauth2" map to TierStatic so nothing breaks.
type MCPAuthConfig struct {
	// Type selects the credential + tier:
	//   none                        — no credential (transport-open)
	//   bearer | oauth2 | static    — TierStatic: per-server static credential,
	//                                 identity-free (the legacy default)
	//   identity | oauth-exchange   — TierOAuth: per-call RFC 8693 delegation token,
	//                                 audience-bound to the server's Audience (#52)
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

	// Audience is the RFC 8707 resource identifier a minted delegation token is
	// bound to when this server is an identity-propagation (TierOAuth) target
	// (change #52). Required for identity/oauth-exchange auth types; ignored for
	// static tiers. Sourced from trusted config, never from model output.
	Audience string `yaml:"audience"`
	// Scopes are the downstream scopes a minted delegation token requests. Sourced
	// from config (the tool declaration), never from model output.
	Scopes []string `yaml:"scopes"`
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

// PipelineSynthesisConfig holds the caps applied to LLM-synthesized pipelines
// (change 0026). Each is a synthesis-time brake mapped onto pipeline.Caps: the
// synthesizer's proposed DAG must fit under these before it is run. 0 = that
// check skipped (matching pipeline.Caps semantics), though Default() supplies
// conservative positive values.
type PipelineSynthesisConfig struct {
	MaxSteps    int `yaml:"max_steps"`
	MaxFanout   int `yaml:"max_fanout"`
	MaxDepth    int `yaml:"max_depth"`
	MaxAttempts int `yaml:"max_attempts"`
}

// PipelineConfig groups pipeline-composition knobs (change 0026). Today it holds
// only the synthesis caps block.
type PipelineConfig struct {
	Synthesis PipelineSynthesisConfig `yaml:"synthesis"`
}

// AuthTokenConfig is one static bearer token→principal mapping for the networked
// loop-control binding (change 0049). Token is the bearer credential a client
// presents in `Authorization: Bearer <token>`; Tenant is the isolation boundary
// the caller acts within (empty ⇒ the storage layer's _default tenant); Subject
// is the authorization subject recorded as a loop's owner. This is the default
// StaticVerifier surface — richer verifiers (OIDC/JWT, mTLS) slot in behind the
// same loopauth.Verifier seam without a config re-cut.
type AuthTokenConfig struct {
	Token                 string `yaml:"token"`
	Tenant                string `yaml:"tenant"`
	Subject               string `yaml:"subject"`
	ObservabilityOperator bool   `yaml:"observability_operator"`
}

// LoopServerConfig configures the networked Connect/protobuf loop-control binding
// (`fuse loop-serve-net`, change 0049). It is inert for every other binding — the
// stdio loop-server and the local CLI paths never reach the Connect edge, so they
// carry no bearer-token requirement and ignore this block entirely.
//
//   - Auth is the static token→principal map the default loopauth.StaticVerifier
//     is built from. When it is empty the composition root synthesizes a single
//     built-in dev token (mapped to the _default tenant) so local use stays usable
//     while STILL requiring a bearer token on every request — the binding never
//     runs unauthenticated. Configure real tokens for any shared/deployed server.
//   - LeaseTTL is the owner-liveness lease duration threaded into runtime.Deps
//     (change 0049 Task 7): a live loop's owner renews the lease at ~⅓ TTL, and a
//     record whose lease has expired is treated as abandoned and re-ownable on a
//     cold resolve. A duration string (e.g. "30s", "2m"); empty ⇒ the runtime's
//     built-in default (30s).
type LoopServerConfig struct {
	Auth     []AuthTokenConfig `yaml:"auth"`
	LeaseTTL string            `yaml:"lease_ttl"`
}

// ObservabilityConfig controls the optional telemetry adapters used by
// loop-serve-net. Every signal is independently enabled; the zero value keeps
// observability inert and requires no collector, metrics listener, or log file.
type ObservabilityConfig struct {
	Metrics     MetricsObservabilityConfig     `yaml:"metrics"`
	Traces      TracesObservabilityConfig      `yaml:"traces"`
	Logging     LoggingObservabilityConfig     `yaml:"logging"`
	Cardinality CardinalityObservabilityConfig `yaml:"cardinality"`
	InstanceID  string                         `yaml:"instance_id"`
}

type MetricsObservabilityConfig struct {
	Enabled          bool                `yaml:"enabled"`
	Path             string              `yaml:"path"`
	Bind             string              `yaml:"bind"`
	Access           string              `yaml:"access"` // authenticated | public
	HistogramBuckets []float64           `yaml:"histogram_buckets"`
	Labels           map[string][]string `yaml:"labels"`
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

type LoggingObservabilityConfig struct {
	Enabled        bool   `yaml:"enabled"`
	Output         string `yaml:"output"` // stdout | file
	File           string `yaml:"file"`
	Level          string `yaml:"level"`
	MaxOverrideTTL string `yaml:"max_override_ttl"`
}

type CardinalityDimensionConfig struct {
	Budget  int      `yaml:"budget"`
	Pinned  []string `yaml:"pinned"`
	Catalog []string `yaml:"catalog"`
}

type CardinalityObservabilityConfig struct {
	HashVersion string                     `yaml:"hash_version"`
	Salt        string                     `yaml:"salt"`
	Tenant      CardinalityDimensionConfig `yaml:"tenant"`
	Model       CardinalityDimensionConfig `yaml:"model"`
	Tool        CardinalityDimensionConfig `yaml:"tool"`
}

// Config is the fully resolved fuse configuration.
type Config struct {
	Gateway    Gateway
	Models     ModelsConfig
	SkillPaths []string
	// MaxTurns is a pointer so an omitted `max_turns` (nil) is distinguishable
	// from an explicit `max_turns: 0`. nil = unset ⇒ the call site applies the
	// context-aware backstop (unlimited in the interactive shell, 100 headless);
	// a non-nil 0 = explicitly unlimited everywhere; a non-nil N>0 caps every
	// context. Same omitted-key discipline as Permissions.SessionAllow. (0038)
	MaxTurns    *int
	MaxTokens   int
	Permissions PermissionsConfig
	MCPServers  []MCPServerConfig
	Research    ResearchConfig
	Agents      AgentsConfig
	// Throughput is the turn-level rate/quota surface (change 0036): global
	// requests-per-minute / tokens-per-minute smoothing plus an optional session
	// token ceiling, with optional per-provider rate overrides. Every numeric is
	// 0 = unlimited/unset, so an absent `throughput:` block is byte-identical to
	// pre-0036 behavior (no gate, no quota). Parsed and carried here; the rate
	// gate (Task 5) and session-quota enforcement (Task 6) consume it.
	Throughput ThroughputConfig
	// Workflows binds an invocable skill to a spawn policy and worker pool,
	// keyed by workflow name. Nil/empty ⇒ no workflow behavior (byte-identical
	// to pre-0034). A skill may embed a default block in its frontmatter; a
	// config-level entry for the same name overrides it, and .fuse.local.yml may
	// only TIGHTEN pool numbers, never loosen them (ADR-0006 trust boundary).
	Workflows map[string]WorkflowConfig
	// Context groups context-management knobs (change 0027). Its summarization
	// block defaults to Tier 2 on with a 0.85 threshold and 2000-token output
	// cap; the summarizer stays inert until a Completer is wired at the call
	// site, so the defaults are behavior-identical to pre-0027 until then.
	Context ContextConfig
	// Pipeline holds pipeline-composition knobs (change 0026). Its synthesis
	// block caps LLM-synthesized DAGs; the untrusted .fuse.local.yml may only
	// TIGHTEN those caps (ADR-0006), exactly like the workflow pool numbers.
	Pipeline PipelineConfig
	// LoopServer configures the networked Connect/protobuf loop-control binding
	// (change 0049): the static bearer-token→principal verifier map and the
	// owner-liveness lease TTL. Inert for every other binding. It is a
	// bearer-credential surface (a token grants access), so it is honored ONLY
	// from the trusted ~/.fuse/config.yml — a repo-plantable .fuse.local.yml must
	// not be able to mint or widen credentials (ADR-0006 trust boundary).
	LoopServer    LoopServerConfig
	Observability ObservabilityConfig
	// ToolIdentity configures the tool/resource identity-propagation egress
	// (change #52): the built-in STS signing key(s) used to mint per-call,
	// audience-bound delegation tokens for identity-propagating MCP servers, and
	// the local principal used off the networked binding (CLI/shell). Inert unless
	// at least one MCP server is configured with an identity/oauth-exchange auth
	// type. Like LoopServer it is a credential surface (a signing key mints
	// downstream-trusted tokens), so it is honored ONLY from the trusted
	// ~/.fuse/config.yml — never from a repo-plantable .fuse.local.yml (ADR-0006).
	ToolIdentity ToolIdentityConfig
}

// ToolIdentityConfig configures change #52's identity-propagation egress seam.
// An empty block leaves the seam un-wired (byte-identical to pre-#52: MCP tools
// use their static per-server token). It becomes active when the composition
// root builds a CredentialSource from it — which it does only when at least one
// MCP server declares an identity-propagating auth type.
type ToolIdentityConfig struct {
	// SigningKey is the symmetric key the built-in STS signs local delegation
	// tokens with, for the default/local tenant. Trusted-config only. When empty,
	// identity-propagating MCP servers cannot mint and are reported at startup.
	SigningKey string `yaml:"signing_key"`
	// TTL is the minted token lifetime (e.g. "5m"); empty ⇒ the STS default.
	TTL string `yaml:"ttl"`
	// LocalSubject is the authorization subject stamped as the loop initiator on
	// the non-networked (CLI/shell) paths, where there is no bearer token to
	// resolve a Principal from. Empty ⇒ "local". The tenant is the default tenant.
	LocalSubject string `yaml:"local_subject"`
}

// WorkflowConfig is one named workflow: the invocable it binds, its subtree
// spawn pool, and its typed worker definitions. Skill names an invocable
// resolved by name (v1: embedded/user markdown skills); the field is
// deliberately form-agnostic so other invocable forms can bind later.
type WorkflowConfig struct {
	Skill   string                  `yaml:"skill"`
	Pool    PoolConfig              `yaml:"pool"`
	Workers map[string]WorkerConfig `yaml:"workers"`
}

// PoolConfig is a workflow subtree's spawn policy. Each dimension is 0 = unset
// (that brake off), matching how AgentTree.SpawnBudget treats max==0 and the
// scheduler's visibility predicate treats a non-positive slot cap.
//
//   - Concurrent (reversible): max children running+pending in the subtree.
//   - Total (permanent): lifetime spawn quota for the subtree.
//   - MaxDepth (static): spawn depth below the workflow root.
//   - Tokens (permanent, change 0036): lifetime token quota for the subtree
//     (0 = unset/unlimited). Exhaustion strips spawn_agent in that subtree only;
//     accounting and enforcement land in Task 6, this field parses and carries.
type PoolConfig struct {
	Concurrent int `yaml:"concurrent"`
	Total      int `yaml:"total"`
	MaxDepth   int `yaml:"max_depth"`
	Tokens     int `yaml:"tokens"`
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

// WorkerConfig is a typed worker: a tool allowlist (a worker whose allowlist
// omits spawn_agent structurally cannot nest) and an optional model pin
// (empty ⇒ inherit the parent's model).
type WorkerConfig struct {
	Tools []string `yaml:"tools"`
	Model string   `yaml:"model"`
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
	// …). 0 selects the agent default (120s). Orchestration tools (spawn_agent,
	// pipeline_run) are always exempt — they await child agents by design.
	ToolTimeoutSeconds int `yaml:"tool_timeout_seconds"`
}

// rawConfig mirrors the on-disk YAML shape before normalization.
type rawConfig struct {
	Gateway     Gateway                `yaml:"gateway"`
	Models      map[string]interface{} `yaml:"models"`
	SkillPaths  []string               `yaml:"skill_paths"`
	MaxTurns    *int                   `yaml:"max_turns"`
	MaxTokens   int                    `yaml:"max_tokens"`
	Permissions rawPermissionsConfig   `yaml:"permissions"`
	MCPServers  []MCPServerConfig      `yaml:"mcp_servers"`
	Research    rawResearchConfig      `yaml:"research"`
	Agents      rawAgentsConfig        `yaml:"agents"`
	// Throughput reuses the resolved ThroughputConfig shape on-disk (plain
	// ints/maps, no free-text scalars — yaml.Unmarshal is safe); the tighten-only
	// merge happens in mergeFile, not here.
	Throughput rawThroughputConfig      `yaml:"throughput"`
	Projects   map[string]ProjectConfig `yaml:"projects"`
	// Workflows reuses the resolved WorkflowConfig shape on-disk (plain
	// maps/lists/ints, no free-text scalars — so yaml.Unmarshal is safe).
	Workflows map[string]WorkflowConfig `yaml:"workflows"`
	// Context mirrors ContextConfig on-disk (change 0027). Its summarization
	// mirror uses a *bool for enabled so an omitted key keeps the true default
	// while `enabled: false` takes effect (mirrors RespectRobots).
	Context rawContextConfig `yaml:"context"`
	// Pipeline mirrors PipelineConfig on-disk (change 0026). Every cap is a plain
	// int (0 = unset), so the tighten-only local merge distinguishes an omitted
	// axis from a present one exactly like the throughput axes; the merge happens
	// in mergeFile.
	Pipeline rawPipelineConfig `yaml:"pipeline"`
	// LoopServer mirrors LoopServerConfig on-disk (change 0049): the bearer
	// token→principal verifier map and the lease TTL for `loop-serve-net`. It is a
	// credential surface honored ONLY from the trusted home file (see mergeFile).
	LoopServer    LoopServerConfig       `yaml:"loop_server"`
	Observability rawObservabilityConfig `yaml:"observability"`
	// ToolIdentity mirrors ToolIdentityConfig on-disk (change #52): the built-in
	// STS signing key + local subject for identity-propagation. A credential
	// surface (the signing key mints downstream-trusted tokens) honored ONLY from
	// the trusted home file (see mergeFile).
	ToolIdentity ToolIdentityConfig `yaml:"tool_identity"`
}

// rawObservabilityConfig preserves YAML presence for trace sample_ratio so an
// explicit zero can disable root-span sampling while an omitted value receives
// the resolved default.
type rawObservabilityConfig struct {
	Metrics     MetricsObservabilityConfig     `yaml:"metrics"`
	Traces      rawTracesObservabilityConfig   `yaml:"traces"`
	Logging     LoggingObservabilityConfig     `yaml:"logging"`
	Cardinality CardinalityObservabilityConfig `yaml:"cardinality"`
	InstanceID  string                         `yaml:"instance_id"`
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
		Metrics: o.Metrics,
		Traces: TracesObservabilityConfig{
			Enabled: o.Traces.Enabled, Endpoint: o.Traces.Endpoint, Protocol: o.Traces.Protocol,
			Insecure: o.Traces.Insecure, Headers: o.Traces.Headers, QueueSize: o.Traces.QueueSize,
			BatchSize: o.Traces.BatchSize, ExportTimeout: o.Traces.ExportTimeout,
			BatchTimeout: o.Traces.BatchTimeout, SampleRatio: ratio,
		},
		Logging: o.Logging, Cardinality: o.Cardinality, InstanceID: o.InstanceID,
	}
}

// rawPipelineConfig mirrors PipelineConfig on-disk (change 0026).
type rawPipelineConfig struct {
	Synthesis rawPipelineSynthesisConfig `yaml:"synthesis"`
}

// rawPipelineSynthesisConfig mirrors PipelineSynthesisConfig on-disk. Every cap
// is a plain int: 0 = unset (an omitted axis), a present value is nonzero.
type rawPipelineSynthesisConfig struct {
	MaxSteps    int `yaml:"max_steps"`
	MaxFanout   int `yaml:"max_fanout"`
	MaxDepth    int `yaml:"max_depth"`
	MaxAttempts int `yaml:"max_attempts"`
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
			MaxSpawns:     64,
			MaxConcurrent: 16,
		},
		// Tier 2 anchored summarization defaults on (change 0027) with the
		// documented threshold/output cap. Model empty ⇒ the main model (D4).
		// The summarizer stays inert until a Completer is wired at the call
		// site, so this default is behavior-identical to pre-0027.
		Context: ContextConfig{
			Summarization: SummarizationConfig{
				Enabled:   true,
				Threshold: 0.85,
				MaxOutput: 2000,
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
		// Pipeline synthesis caps (change 0026): conservative brakes on an
		// LLM-synthesized DAG. These map onto pipeline.Caps at the wiring site.
		// The untrusted .fuse.local.yml may only TIGHTEN them (ADR-0006).
		Pipeline: PipelineConfig{
			Synthesis: PipelineSynthesisConfig{
				MaxSteps:    50,
				MaxFanout:   10,
				MaxDepth:    4,
				MaxAttempts: 3,
			},
		},
		// The research workflow ships as a built-in default (change 0034): it
		// binds the research skill to a facet-researcher worker (no spawn_agent,
		// so it cannot nest) and a {concurrent:5, total:8, max_depth:1} pool — a
		// reservation within the global brakes. A config-level workflows.research
		// entry overrides these via the normal per-field merge.
		Workflows: map[string]WorkflowConfig{
			"research": {
				Skill: "research",
				Pool:  PoolConfig{Concurrent: 5, Total: 8, MaxDepth: 1},
				Workers: map[string]WorkerConfig{
					"facet-researcher": {
						Tools: []string{"web_search", "web_fetch", "read_file"},
					},
				},
			},
		},
	}
}
