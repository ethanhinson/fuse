// Package harnessconfig maps between the fuse.harness.v1 HarnessSpec (the
// portable, client-settable harness specification, change 0084 / ADR-0060) and
// the runtime's resolved config.Config.
//
// internal/config stays the authority for semantics. ToConfig does not
// re-implement the loader's merge: it renders the spec as the YAML document the
// loader already understands and resolves it through config.FromYAML - the
// loader's own trusted merge over Default() - so defaults, the 0-means-unset
// discipline, the optional-bool handling and the per-field workflow merge are
// exactly the loader's. FromConfig is the inverse for the client-settable
// surface: it emits only what differs from config.Default() (so a spec carries
// intent, not a snapshot of today's defaults) and reports, never carries, every
// operator-only setting and every inline secret value.
//
// The package is internal on purpose: its signatures name config.Config, which
// no other Go module can import. The public surface is the validator in
// github.com/ethanhinson/fuse/harness.
package harnessconfig

import (
	"fmt"
	"math"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"google.golang.org/protobuf/reflect/protoreflect"
	"gopkg.in/yaml.v3"

	harnessv1 "github.com/ethanhinson/fuse/harness/v1"
	"github.com/ethanhinson/fuse/internal/config"
)

// SecretResolver turns a secret_ref NAME into its value. A nil resolver means
// PlaceholderSecrets.
type SecretResolver func(name string) (string, error)

// secretPlaceholderPrefix marks a config string that stands for a secret_ref
// rather than holding a value.
const secretPlaceholderPrefix = "secret-ref:"

// SecretPlaceholder is the config-side stand-in for a secret_ref: what
// PlaceholderSecrets returns and what FromConfig recognizes as a reference.
func SecretPlaceholder(name string) string { return secretPlaceholderPrefix + name }

// PlaceholderSecrets resolves every ref to its SecretPlaceholder, so a spec can
// be mapped (for validation, or a lossless round trip) without any secret value
// ever being read.
func PlaceholderSecrets(name string) (string, error) { return SecretPlaceholder(name), nil }

// Enum <-> config-string tables. The config strings are the loader's values.
var (
	modeToConfig = map[harnessv1.PermissionMode]string{
		harnessv1.PermissionMode_PERMISSION_MODE_OFF:        "off",
		harnessv1.PermissionMode_PERMISSION_MODE_PROMPT_ALL: "prompt-all",
		harnessv1.PermissionMode_PERMISSION_MODE_SMART:      "smart",
		harnessv1.PermissionMode_PERMISSION_MODE_AUTO:       "auto",
	}
	transportToConfig = map[harnessv1.McpTransport]string{
		harnessv1.McpTransport_MCP_TRANSPORT_HTTP:            "http",
		harnessv1.McpTransport_MCP_TRANSPORT_SSE:             "sse",
		harnessv1.McpTransport_MCP_TRANSPORT_STREAMABLE_HTTP: "streamable-http",
	}
	authToConfig = map[harnessv1.McpAuthType]string{
		harnessv1.McpAuthType_MCP_AUTH_TYPE_BEARER:         "bearer",
		harnessv1.McpAuthType_MCP_AUTH_TYPE_OAUTH2:         "oauth2",
		harnessv1.McpAuthType_MCP_AUTH_TYPE_STATIC:         "static",
		harnessv1.McpAuthType_MCP_AUTH_TYPE_IDENTITY:       "identity",
		harnessv1.McpAuthType_MCP_AUTH_TYPE_OAUTH_EXCHANGE: "oauth-exchange",
	}
	providerToConfig = map[harnessv1.ResearchProvider]string{
		harnessv1.ResearchProvider_RESEARCH_PROVIDER_BRAVE:  "brave",
		harnessv1.ResearchProvider_RESEARCH_PROVIDER_TAVILY: "tavily",
		harnessv1.ResearchProvider_RESEARCH_PROVIDER_CUSTOM: "custom",
	}
)

func invert[K comparable, V comparable](m map[K]V) map[V]K {
	out := make(map[V]K, len(m))
	for k, v := range m {
		out[v] = k
	}
	return out
}

var (
	modeFromConfig      = invert(modeToConfig)
	transportFromConfig = invert(transportToConfig)
	authFromConfig      = invert(authToConfig)
	providerFromConfig  = invert(providerToConfig)
)

// ToConfig resolves spec into the config the runtime would run with: the spec
// rendered as a config document and merged over Default() by the loader itself
// (config.FromYAML). A nil spec is the empty spec, which resolves to Default().
// secret_ref fields are resolved through secrets (nil: PlaceholderSecrets).
//
// ToConfig is total over shape errors: an undefined enum number or an
// out-of-range value is carried or dropped, never rejected, because shape is
// protovalidate's job (github.com/ethanhinson/fuse/harness runs both).
// SkillRefs have no config.Config counterpart and are not mapped: a server
// resolves them against its installed skills (internal/harnessresolve, change
// 0085).
func ToConfig(spec *harnessv1.HarnessSpec, secrets SecretResolver) (config.Config, error) {
	if secrets == nil {
		secrets = PlaceholderSecrets
	}
	doc, err := toDocument(spec, secrets)
	if err != nil {
		return config.Config{}, err
	}
	data, err := yaml.Marshal(doc)
	if err != nil {
		return config.Config{}, fmt.Errorf("harness spec: render config document: %w", err)
	}
	return config.FromYAML(data, "harness spec")
}

// doc is a YAML mapping under construction; set skips zero values so the
// rendered document only names what the spec set (the loader treats an omitted
// key and a zero value identically anyway, except where a pointer says so).
type doc map[string]any

func (d doc) set(key string, v any) {
	switch x := v.(type) {
	case string:
		if x == "" {
			return
		}
	case uint32:
		if x == 0 {
			return
		}
	case float64:
		if x == 0 {
			return
		}
	case bool:
		if !x {
			return
		}
	case []string:
		if len(x) == 0 {
			return
		}
	case doc:
		if len(x) == 0 {
			return
		}
	case []any:
		if len(x) == 0 {
			return
		}
	}
	d[key] = v
}

func toDocument(s *harnessv1.HarnessSpec, secrets SecretResolver) (doc, error) {
	root := doc{}
	if s == nil {
		return root, nil
	}

	models := doc{}
	models.set("default", s.GetModels().GetDefaultAlias())
	for alias, m := range s.GetModels().GetEntries() {
		e := doc{}
		e.set("id", m.GetId())
		e.set("max_tokens", m.GetMaxTokens())
		e.set("context_window", m.GetContextWindow())
		e.set("persona", m.GetPersona())
		e.set("system_prefix", m.GetSystemPrefix())
		models[alias] = e
	}
	root.set("models", models)

	var servers []any
	for i, srv := range s.GetMcpServers() {
		d := doc{}
		d.set("name", srv.GetName())
		d.set("transport", transportToConfig[srv.GetTransport()])
		d.set("url", srv.GetUrl())
		auth := doc{}
		auth.set("type", authToConfig[srv.GetAuth().GetType()])
		auth.set("client_id", srv.GetAuth().GetClientId())
		if ref := srv.GetAuth().GetClientSecretRef(); ref != "" {
			v, err := secrets(ref)
			if err != nil {
				return nil, fmt.Errorf("harness spec: mcp_servers[%d].auth.client_secret_ref %q: %w", i, ref, err)
			}
			auth.set("client_secret", v)
		}
		auth.set("scopes", srv.GetAuth().GetScopes())
		d.set("auth", auth)
		d.set("audience", srv.GetAudience())
		d.set("scopes", srv.GetScopes())
		servers = append(servers, d)
	}
	root.set("mcp_servers", servers)

	p := s.GetPermissions()
	perms := doc{}
	perms.set("mode", modeToConfig[p.GetMode()])
	if p != nil && p.SessionAllow != nil {
		perms["session_allow"] = p.GetSessionAllow()
	}
	perms.set("auto_approve", p.GetAutoApprove())
	perms.set("always_prompt", p.GetAlwaysPrompt())
	perms.set("disabled", p.GetDisabled())
	auto := doc{}
	auto.set("classifier_model", p.GetAuto().GetClassifierModel())
	auto.set("deny", p.GetAuto().GetDeny())
	auto.set("ask", p.GetAuto().GetAsk())
	auto.set("fetch_deny", p.GetAuto().GetFetchDeny())
	auto.set("fetch_ask", p.GetAuto().GetFetchAsk())
	auto.set("allow_push", p.GetAuto().GetAllowPush())
	perms.set("auto", auto)
	root.set("permissions", perms)

	orch := doc{}
	orch.set("model", s.GetOrchestrator().GetModel())
	root.set("orchestrator", orch)

	agents := doc{}
	agents.set("max_spawns", s.GetAgents().GetMaxSpawns())
	agents.set("max_concurrent", s.GetAgents().GetMaxConcurrent())
	agents.set("queue_bound", s.GetAgents().GetQueueBound())
	agents.set("tool_timeout_seconds", s.GetAgents().GetToolTimeoutSeconds())
	root.set("agents", agents)

	workflows := doc{}
	for name, w := range s.GetWorkflows() {
		d := doc{}
		d.set("skill", w.GetSkill())
		pool := doc{}
		pool.set("concurrent", w.GetPool().GetConcurrent())
		pool.set("total", w.GetPool().GetTotal())
		pool.set("max_depth", w.GetPool().GetMaxDepth())
		pool.set("tokens", w.GetPool().GetTokens())
		d.set("pool", pool)
		workers := doc{}
		for wn, wk := range w.GetWorkers() {
			wd := doc{}
			wd.set("tools", wk.GetTools())
			wd.set("model", wk.GetModel())
			workers[wn] = wd
		}
		d.set("workers", workers)
		workflows[name] = d
	}
	root.set("workflows", workflows)

	tp := s.GetThroughput()
	through := doc{}
	through.set("requests_per_minute", tp.GetRequestsPerMinute())
	through.set("tokens_per_minute", tp.GetTokensPerMinute())
	through.set("session_tokens", tp.GetSessionTokens())
	providers := doc{}
	for name, pt := range tp.GetProviders() {
		d := doc{}
		d.set("requests_per_minute", pt.GetRequestsPerMinute())
		d.set("tokens_per_minute", pt.GetTokensPerMinute())
		providers[name] = d
	}
	through.set("providers", providers)
	root.set("throughput", through)

	sum := s.GetContext().GetSummarization()
	sd := doc{}
	if sum != nil && sum.Enabled != nil {
		sd["enabled"] = sum.GetEnabled()
	}
	sd.set("model", sum.GetModel())
	sd.set("threshold", sum.GetThreshold())
	sd.set("max_output", sum.GetMaxOutput())
	rel := s.GetContext().GetRelevance()
	rd := doc{}
	if rel != nil && rel.Heuristic != nil {
		rd["heuristic"] = rel.GetHeuristic()
	}
	rd.set("recency_floor_pct", rel.GetRecencyFloorPct())
	rd.set("body_scan_bytes", rel.GetBodyScanBytes())
	rd.set("classifier_model", rel.GetClassifierModel())
	rd.set("classifier_batch_size", rel.GetClassifierBatchSize())
	rd.set("borderline_lo", rel.GetBorderlineLo())
	rd.set("borderline_hi", rel.GetBorderlineHi())
	ctx := doc{}
	ctx.set("summarization", sd)
	ctx.set("relevance", rd)
	root.set("context", ctx)

	syn := s.GetPipeline().GetSynthesis()
	sy := doc{}
	sy.set("max_steps", syn.GetMaxSteps())
	sy.set("max_fanout", syn.GetMaxFanout())
	sy.set("max_depth", syn.GetMaxDepth())
	sy.set("max_attempts", syn.GetMaxAttempts())
	pipe := doc{}
	pipe.set("synthesis", sy)
	root.set("pipeline", pipe)

	if s.MaxTurns != nil {
		root["max_turns"] = s.GetMaxTurns()
	}
	root.set("max_tokens", s.GetMaxTokens())

	r := s.GetResearch()
	rs := doc{}
	rs.set("provider", providerToConfig[r.GetProvider()])
	rs.set("max_queries", r.GetMaxQueries())
	rs.set("max_results", r.GetMaxResults())
	rs.set("max_content_kb", r.GetMaxContentKb())
	if r != nil && r.RespectRobots != nil {
		rs["respect_robots"] = r.GetRespectRobots()
	}
	c := r.GetCustom()
	cd := doc{}
	cd.set("url", c.GetUrl())
	headers := doc{}
	for name, hv := range c.GetHeaders() {
		switch v := hv.GetValue().(type) {
		case *harnessv1.HeaderValue_Literal:
			headers[name] = v.Literal
		case *harnessv1.HeaderValue_SecretRef:
			val, err := secrets(v.SecretRef)
			if err != nil {
				return nil, fmt.Errorf("harness spec: research.custom.headers[%q].secret_ref %q: %w", name, v.SecretRef, err)
			}
			headers[name] = val
		}
	}
	cd.set("headers", headers)
	cd.set("results_path", c.GetResultsPath())
	cd.set("title_field", c.GetTitleField())
	cd.set("url_field", c.GetUrlField())
	cd.set("snippet_field", c.GetSnippetField())
	rs.set("custom", cd)
	root.set("research", rs)

	return root, nil
}

// Omission is a config setting FromConfig did not carry into the spec. Path is
// the config key path (list elements as [i]); Reason says why.
type Omission struct {
	Path   string
	Reason string
}

const (
	reasonOperator = "operator-only setting; a harness spec cannot express it (ADR-0006, ADR-0060)"
	reasonSecret   = "inline secret value; a harness spec carries only secret_ref names - provision a secret and reference it"
)

// FromConfig derives the HarnessSpec for the client-settable part of cfg. It
// emits only values that differ from config.Default(), so ToConfig(FromConfig(c))
// reproduces c for any c the loader can produce from client-settable keys.
// Everything it cannot carry - operator-only blocks, stdio MCP servers, host
// paths, inline secret values, values outside the spec's enums - is returned as
// an Omission instead.
func FromConfig(cfg config.Config) (*harnessv1.HarnessSpec, []Omission) {
	def := config.Default()
	var om []Omission
	omit := func(path, reason string) { om = append(om, Omission{Path: path, Reason: reason}) }
	// num narrows a config int to the spec's unsigned field. A negative value
	// has no spec form (and is invalid or clamped in config), so it is reported
	// rather than silently becoming 0, which means "runtime default" in a spec.
	num := func(path string, n int) uint32 {
		if n < 0 {
			omit(path, "negative value; the spec field is unsigned")
			return 0
		}
		if uint64(n) > math.MaxUint32 {
			omit(path, "value exceeds the spec field's uint32 range")
			return 0
		}
		return uint32(n)
	}
	s := &harnessv1.HarnessSpec{}

	// Operator-only blocks.
	if cfg.Gateway != def.Gateway {
		omit("gateway", reasonOperator)
	}
	if len(cfg.SkillPaths) > 0 {
		omit("skill_paths", "host directories; a harness spec references skills by name (skills)")
	}
	if !reflect.DeepEqual(cfg.LoopServer, def.LoopServer) {
		omit("loop_server", reasonOperator)
	}
	if cfg.ToolIdentity != def.ToolIdentity {
		omit("tool_identity", reasonOperator)
	}
	if !reflect.DeepEqual(cfg.Observability, def.Observability) {
		omit("observability", reasonOperator)
	}

	// Models.
	m := &harnessv1.Models{}
	if cfg.Models.Default != def.Models.Default {
		m.DefaultAlias = cfg.Models.Default
	}
	for alias, mc := range cfg.Models.Entries {
		if m.Entries == nil {
			m.Entries = map[string]*harnessv1.Model{}
		}
		m.Entries[alias] = &harnessv1.Model{
			Id: mc.ID, MaxTokens: num("models."+alias+".max_tokens", mc.MaxTokens), ContextWindow: num("models."+alias+".context_window", mc.ContextWindow),
			Persona: mc.Persona, SystemPrefix: mc.SystemPrefix,
		}
		// Advertising metadata is the operator's catalogue; HarnessSpec has no
		// field for it.
		if mc.Label != "" {
			omit("models."+alias+".label", reasonOperator)
		}
		if mc.Description != "" {
			omit("models."+alias+".description", reasonOperator)
		}
		if len(mc.Tags) > 0 {
			omit("models."+alias+".tags", reasonOperator)
		}
		if mc.Advertise != nil {
			omit("models."+alias+".advertise", reasonOperator)
		}
	}
	if m.DefaultAlias != "" || len(m.Entries) > 0 {
		s.Models = m
	}

	// MCP servers: remote transports only.
	for i, srv := range cfg.MCPServers {
		at := fmt.Sprintf("mcp_servers[%d]", i)
		tr, ok := transportFromConfig[srv.Transport]
		if !ok {
			omit(at, fmt.Sprintf("transport %q runs a process on the runtime host; only remote (http, sse, streamable-http) servers are client-settable", srv.Transport))
			continue
		}
		out := &harnessv1.McpServer{Name: srv.Name, Transport: tr, Url: srv.URL, Audience: srv.Audience, Scopes: srv.Scopes}
		// command/env only configure a stdio host process; on a remote server they
		// are inert, but still reported rather than silently dropped.
		if len(srv.Command) > 0 {
			omit(at+".command", reasonOperator)
		}
		if len(srv.Env) > 0 {
			omit(at+".env", reasonOperator)
		}
		auth := &harnessv1.McpAuth{ClientId: srv.Auth.ClientID, Scopes: srv.Auth.Scopes}
		if srv.Auth.Type != "" && srv.Auth.Type != "none" {
			if t, ok := authFromConfig[srv.Auth.Type]; ok {
				auth.Type = t
			} else {
				omit(at+".auth.type", fmt.Sprintf("auth type %q has no HarnessSpec equivalent", srv.Auth.Type))
			}
		}
		if cs := srv.Auth.ClientSecret; cs != "" {
			if ref, ok := strings.CutPrefix(cs, secretPlaceholderPrefix); ok {
				auth.ClientSecretRef = ref
			} else {
				omit(at+".auth.client_secret", reasonSecret)
			}
		}
		if srv.Auth.TokenFile != "" {
			omit(at+".auth.token_file", "host path; operator-only")
		}
		if auth.Type != 0 || auth.ClientId != "" || auth.ClientSecretRef != "" || len(auth.Scopes) > 0 {
			out.Auth = auth
		}
		s.McpServers = append(s.McpServers, out)
	}

	// Permissions.
	p, dp := cfg.Permissions, def.Permissions
	perms := &harnessv1.Permissions{AutoApprove: p.AutoApprove, AlwaysPrompt: p.AlwaysPrompt, Disabled: p.Disabled}
	if p.Mode != dp.Mode && p.Mode != "" {
		if mode, ok := modeFromConfig[p.Mode]; ok {
			perms.Mode = mode
		} else {
			omit("permissions.mode", fmt.Sprintf("mode %q has no HarnessSpec equivalent", p.Mode))
		}
	}
	if p.SessionAllow != dp.SessionAllow {
		perms.SessionAllow = ptr(p.SessionAllow)
	}
	auto := &harnessv1.AutoPermissions{
		ClassifierModel: p.Auto.ClassifierModel, Deny: p.Auto.Deny, Ask: p.Auto.Ask,
		FetchDeny: p.Auto.FetchDeny, FetchAsk: p.Auto.FetchAsk, AllowPush: p.Auto.AllowPush,
	}
	if len(p.Auto.WriteRoots) > 0 {
		omit("permissions.auto.write_roots", "host directories; operator-only")
	}
	if !isZero(auto) {
		perms.Auto = auto
	}
	if !isZero(perms) {
		s.Permissions = perms
	}

	if cfg.Orchestrator.Model != def.Orchestrator.Model {
		s.Orchestrator = &harnessv1.Orchestrator{Model: cfg.Orchestrator.Model}
	}

	a, da := cfg.Agents, def.Agents
	agents := &harnessv1.Agents{}
	if a.MaxSpawns != da.MaxSpawns {
		agents.MaxSpawns = num("agents.max_spawns", a.MaxSpawns)
	}
	if a.MaxConcurrent != da.MaxConcurrent {
		agents.MaxConcurrent = num("agents.max_concurrent", a.MaxConcurrent)
	}
	if a.QueueBound != da.QueueBound {
		agents.QueueBound = a.QueueBound
	}
	if a.ToolTimeoutSeconds != da.ToolTimeoutSeconds {
		agents.ToolTimeoutSeconds = num("agents.tool_timeout_seconds", a.ToolTimeoutSeconds)
	}
	if !isZero(agents) {
		s.Agents = agents
	}

	// Workflows: a built-in workflow carries only the fields that differ from
	// its default (the loader merges a workflow entry per field).
	for name, w := range cfg.Workflows {
		dw, builtin := def.Workflows[name]
		out := &harnessv1.Workflow{}
		if !builtin || w.Skill != dw.Skill {
			out.Skill = w.Skill
		}
		pool := &harnessv1.Pool{}
		if !builtin || w.Pool.Concurrent != dw.Pool.Concurrent {
			pool.Concurrent = num("workflows."+name+".pool.concurrent", w.Pool.Concurrent)
		}
		if !builtin || w.Pool.Total != dw.Pool.Total {
			pool.Total = num("workflows."+name+".pool.total", w.Pool.Total)
		}
		if !builtin || w.Pool.MaxDepth != dw.Pool.MaxDepth {
			pool.MaxDepth = num("workflows."+name+".pool.max_depth", w.Pool.MaxDepth)
		}
		if !builtin || w.Pool.Tokens != dw.Pool.Tokens {
			pool.Tokens = num("workflows."+name+".pool.tokens", w.Pool.Tokens)
		}
		if !isZero(pool) {
			out.Pool = pool
		}
		if !builtin || !reflect.DeepEqual(w.Workers, dw.Workers) {
			for wn, wk := range w.Workers {
				if out.Workers == nil {
					out.Workers = map[string]*harnessv1.Worker{}
				}
				out.Workers[wn] = &harnessv1.Worker{Tools: wk.Tools, Model: wk.Model}
			}
		}
		if !builtin || !isZero(out) {
			if s.Workflows == nil {
				s.Workflows = map[string]*harnessv1.Workflow{}
			}
			s.Workflows[name] = out
		}
	}

	t := cfg.Throughput
	tp := &harnessv1.Throughput{
		RequestsPerMinute: num("throughput.requests_per_minute", t.RequestsPerMinute), TokensPerMinute: num("throughput.tokens_per_minute", t.TokensPerMinute), SessionTokens: num("throughput.session_tokens", t.SessionTokens),
	}
	for name, pt := range t.Providers {
		if tp.Providers == nil {
			tp.Providers = map[string]*harnessv1.ProviderThroughput{}
		}
		tp.Providers[name] = &harnessv1.ProviderThroughput{RequestsPerMinute: num("throughput.providers."+name+".requests_per_minute", pt.RequestsPerMinute), TokensPerMinute: num("throughput.providers."+name+".tokens_per_minute", pt.TokensPerMinute)}
	}
	if !isZero(tp) {
		s.Throughput = tp
	}

	cs, dcs := cfg.Context.Summarization, def.Context.Summarization
	sum := &harnessv1.Summarization{}
	if cs.Enabled != dcs.Enabled {
		sum.Enabled = ptr(cs.Enabled)
	}
	if cs.Model != dcs.Model {
		sum.Model = cs.Model
	}
	if cs.Threshold != dcs.Threshold {
		sum.Threshold = cs.Threshold
	}
	if cs.MaxOutput != dcs.MaxOutput {
		sum.MaxOutput = num("context.summarization.max_output", cs.MaxOutput)
	}
	cr, dcr := cfg.Context.Relevance, def.Context.Relevance
	rel := &harnessv1.Relevance{}
	if cr.Heuristic != dcr.Heuristic {
		rel.Heuristic = ptr(cr.Heuristic)
	}
	if cr.RecencyFloorPct != dcr.RecencyFloorPct {
		rel.RecencyFloorPct = num("context.relevance.recency_floor_pct", cr.RecencyFloorPct)
	}
	if cr.BodyScanBytes != dcr.BodyScanBytes {
		rel.BodyScanBytes = num("context.relevance.body_scan_bytes", cr.BodyScanBytes)
	}
	if cr.ClassifierModel != dcr.ClassifierModel {
		rel.ClassifierModel = cr.ClassifierModel
	}
	if cr.ClassifierBatchSize != dcr.ClassifierBatchSize {
		rel.ClassifierBatchSize = num("context.relevance.classifier_batch_size", cr.ClassifierBatchSize)
	}
	if cr.BorderlineLo != dcr.BorderlineLo {
		rel.BorderlineLo = cr.BorderlineLo
	}
	if cr.BorderlineHi != dcr.BorderlineHi {
		rel.BorderlineHi = cr.BorderlineHi
	}
	ctx := &harnessv1.Context{}
	if !isZero(sum) {
		ctx.Summarization = sum
	}
	if !isZero(rel) {
		ctx.Relevance = rel
	}
	if !isZero(ctx) {
		s.Context = ctx
	}

	ps, dps := cfg.Pipeline.Synthesis, def.Pipeline.Synthesis
	syn := &harnessv1.PipelineSynthesis{}
	if ps.MaxSteps != dps.MaxSteps {
		syn.MaxSteps = num("pipeline.synthesis.max_steps", ps.MaxSteps)
	}
	if ps.MaxFanout != dps.MaxFanout {
		syn.MaxFanout = num("pipeline.synthesis.max_fanout", ps.MaxFanout)
	}
	if ps.MaxDepth != dps.MaxDepth {
		syn.MaxDepth = num("pipeline.synthesis.max_depth", ps.MaxDepth)
	}
	if ps.MaxAttempts != dps.MaxAttempts {
		syn.MaxAttempts = num("pipeline.synthesis.max_attempts", ps.MaxAttempts)
	}
	if !isZero(syn) {
		s.Pipeline = &harnessv1.Pipeline{Synthesis: syn}
	}

	if cfg.MaxTurns != nil {
		if *cfg.MaxTurns < 0 {
			omit("max_turns", "negative value; the spec field is unsigned (0 = unlimited)")
		} else {
			s.MaxTurns = ptr(uint32(*cfg.MaxTurns))
		}
	}
	if cfg.MaxTokens != def.MaxTokens {
		s.MaxTokens = num("max_tokens", cfg.MaxTokens)
	}

	r, dr := cfg.Research, def.Research
	res := &harnessv1.Research{}
	if r.Provider != dr.Provider {
		if pv, ok := providerFromConfig[r.Provider]; ok {
			res.Provider = pv
		} else {
			omit("research.provider", fmt.Sprintf("provider %q has no HarnessSpec equivalent", r.Provider))
		}
	}
	if r.MaxQueries != dr.MaxQueries {
		res.MaxQueries = num("research.max_queries", r.MaxQueries)
	}
	if r.MaxResults != dr.MaxResults {
		res.MaxResults = num("research.max_results", r.MaxResults)
	}
	if r.MaxContentKB != dr.MaxContentKB {
		res.MaxContentKb = num("research.max_content_kb", r.MaxContentKB)
	}
	if r.RespectRobots != dr.RespectRobots {
		res.RespectRobots = ptr(r.RespectRobots)
	}
	c, dc := r.Custom, dr.Custom
	custom := &harnessv1.CustomSearchProvider{}
	if c.URL != dc.URL {
		custom.Url = c.URL
	}
	names := make([]string, 0, len(c.Headers))
	for name := range c.Headers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		v := c.Headers[name]
		var hv *harnessv1.HeaderValue
		if ref, ok := strings.CutPrefix(v, secretPlaceholderPrefix); ok {
			hv = &harnessv1.HeaderValue{Value: &harnessv1.HeaderValue_SecretRef{SecretRef: ref}}
		} else if credentialHeader(name) {
			omit("research.custom.headers."+name, reasonSecret)
			continue
		} else {
			hv = &harnessv1.HeaderValue{Value: &harnessv1.HeaderValue_Literal{Literal: v}}
		}
		if custom.Headers == nil {
			custom.Headers = map[string]*harnessv1.HeaderValue{}
		}
		custom.Headers[name] = hv
	}
	if c.ResultsPath != dc.ResultsPath {
		custom.ResultsPath = c.ResultsPath
	}
	if c.TitleField != dc.TitleField {
		custom.TitleField = c.TitleField
	}
	if c.URLField != dc.URLField {
		custom.UrlField = c.URLField
	}
	if c.SnippetField != dc.SnippetField {
		custom.SnippetField = c.SnippetField
	}
	if !isZero(custom) {
		res.Custom = custom
	}
	if !isZero(res) {
		s.Research = res
	}

	return s, om
}

// credentialHeaderPattern matches header names that conventionally carry a
// credential. It is the same expression as the
// fuse.headers.credential_literal CEL rule in harness.proto (pinned by a test):
// the spec rejects a literal value for such a header, and FromConfig never
// copies one into a spec.
const credentialHeaderPattern = `(?i)(auth|key|token|secret|password|^cookie$)`

var credentialHeaderRE = regexp.MustCompile(credentialHeaderPattern)

// credentialHeader reports whether a header name conventionally carries a
// credential.
func credentialHeader(name string) bool { return credentialHeaderRE.MatchString(name) }

// SpecPath translates a config key path (as config.Problem.Path reports it)
// into the HarnessSpec field path a form places the error against. It returns
// "" when the config key has no spec field (an operator-only surface), so a
// caller never points a client at a field it cannot see.
func SpecPath(configPath string) string {
	p := configPath
	if p == "models.default" {
		p = "models.default_alias"
	}
	md := (&harnessv1.HarnessSpec{}).ProtoReflect().Descriptor()
	parts := strings.Split(p, ".")
	for i, part := range parts {
		fd := md.Fields().ByName(protoreflect.Name(part))
		if fd == nil {
			return ""
		}
		if i < len(parts)-1 {
			if fd.Message() == nil || fd.IsMap() || fd.IsList() {
				return ""
			}
			md = fd.Message()
		}
	}
	return p
}

func ptr[T any](v T) *T { return &v }

// isZero reports whether a generated message has no field set.
func isZero(m interface{ ProtoReflect() protoreflect.Message }) bool {
	empty := true
	m.ProtoReflect().Range(func(protoreflect.FieldDescriptor, protoreflect.Value) bool {
		empty = false
		return false
	})
	return empty
}
