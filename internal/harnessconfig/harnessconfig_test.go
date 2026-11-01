package harnessconfig

import (
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"

	"buf.build/gen/go/bufbuild/protovalidate/protocolbuffers/go/buf/validate"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	harnessv1 "github.com/ethanhinson/fuse/harness/v1"
	"github.com/ethanhinson/fuse/internal/config"
)

// tricky is a task brief that a naive YAML rendering would mangle: a
// "key: value" line, a leading "#", a list marker, mixed quotes and a tab.
const tricky = "You are a release-notes writer.\nKey: value pairs stay literal.\n# not a comment\n- not a list\n  indented 'single' and \"double\" quotes\ttab"

// fullSpec sets every client-settable field to a NON-default value, so a
// spec -> config -> spec round trip must reproduce it exactly.
func fullSpec() *harnessv1.HarnessSpec {
	f := false
	turns := uint32(0) // explicit unlimited
	return &harnessv1.HarnessSpec{
		Models: &harnessv1.Models{
			DefaultAlias: "fast",
			Entries: map[string]*harnessv1.Model{
				"fast": {Id: "cloud/fast-1", MaxTokens: 4096, ContextWindow: 64000, Persona: "coding", SystemPrefix: "/no_think"},
				"slow": {Id: "cloud/slow-1"},
			},
		},
		McpServers: []*harnessv1.McpServer{
			{
				Name: "rentals", Transport: harnessv1.McpTransport_MCP_TRANSPORT_STREAMABLE_HTTP, Url: "https://mcp.example.com/mcp",
				Auth:     &harnessv1.McpAuth{Type: harnessv1.McpAuthType_MCP_AUTH_TYPE_BEARER, ClientId: "cid", ClientSecretRef: "rentals-token", Scopes: []string{"a"}},
				Audience: "https://mcp.example.com", Scopes: []string{"read"},
			},
			{Name: "plain", Transport: harnessv1.McpTransport_MCP_TRANSPORT_SSE, Url: "https://sse.example.com"},
		},
		Permissions: &harnessv1.Permissions{
			Mode: harnessv1.PermissionMode_PERMISSION_MODE_AUTO, SessionAllow: &f,
			AutoApprove: []string{"make test"}, AlwaysPrompt: []string{"rm *"}, Disabled: []string{"bash"},
			Auto: &harnessv1.AutoPermissions{
				ClassifierModel: "fast", Deny: []string{"curl *"}, Ask: []string{"git reset *"},
				FetchDeny: []string{"*.internal"}, FetchAsk: []string{"*.corp"}, AllowPush: true,
			},
		},
		Orchestrator: &harnessv1.Orchestrator{Model: "slow"},
		Agents:       &harnessv1.Agents{MaxSpawns: 10, MaxConcurrent: 4, QueueBound: 1.5, ToolTimeoutSeconds: 30},
		Workflows: map[string]*harnessv1.Workflow{
			// A partial override of the built-in research workflow.
			"research": {Pool: &harnessv1.Pool{Total: 3}},
			"review": {
				Skill: "review",
				Pool:  &harnessv1.Pool{Concurrent: 2, Total: 6, MaxDepth: 1, Tokens: 100000},
				Workers: map[string]*harnessv1.Worker{
					"reader": {Tools: []string{"read_file"}, Model: "fast"},
				},
			},
		},
		Throughput: &harnessv1.Throughput{
			RequestsPerMinute: 60, TokensPerMinute: 100000, SessionTokens: 5000000,
			Providers: map[string]*harnessv1.ProviderThroughput{"openrouter": {RequestsPerMinute: 20, TokensPerMinute: 50000}},
		},
		Context: &harnessv1.Context{
			Summarization: &harnessv1.Summarization{Enabled: &f, Model: "slow", Threshold: 0.7, MaxOutput: 1000},
			Relevance: &harnessv1.Relevance{
				Heuristic: &f, RecencyFloorPct: 40, BodyScanBytes: 4096, ClassifierModel: "fast",
				ClassifierBatchSize: 5, BorderlineLo: 0.2, BorderlineHi: 0.5,
			},
		},
		Pipeline:     &harnessv1.Pipeline{Synthesis: &harnessv1.PipelineSynthesis{MaxSteps: 20, MaxFanout: 5, MaxDepth: 2, MaxAttempts: 2}},
		MaxTurns:     &turns,
		MaxTokens:    8192,
		Instructions: tricky,
		Research: &harnessv1.Research{
			Provider: harnessv1.ResearchProvider_RESEARCH_PROVIDER_CUSTOM, MaxQueries: 3, MaxResults: 4, MaxContentKb: 20, RespectRobots: &f,
			Custom: &harnessv1.CustomSearchProvider{
				Url: "https://search.example.com/search",
				Headers: map[string]*harnessv1.HeaderValue{
					"X-Client":      {Value: &harnessv1.HeaderValue_Literal{Literal: "fuse"}},
					"Authorization": {Value: &harnessv1.HeaderValue_SecretRef{SecretRef: "search-auth"}},
				},
				ResultsPath: "data.items", TitleField: "name", UrlField: "link", SnippetField: "text",
			},
		},
	}
}

func TestToConfigEmptySpecIsDefault(t *testing.T) {
	for _, spec := range []*harnessv1.HarnessSpec{nil, {}} {
		cfg, err := ToConfig(spec, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(cfg, config.Default()) {
			t.Fatalf("empty spec config = %+v, want Default()", cfg)
		}
	}
}

func TestToConfigMapsFields(t *testing.T) {
	cfg, err := ToConfig(fullSpec(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Models.Default != "fast" || cfg.Models.Entries["fast"].SystemPrefix != "/no_think" {
		t.Errorf("models = %+v", cfg.Models)
	}
	if len(cfg.MCPServers) != 2 || cfg.MCPServers[0].Transport != "streamable-http" || cfg.MCPServers[1].Transport != "sse" ||
		cfg.MCPServers[0].Auth.Type != "bearer" || cfg.MCPServers[0].Audience != "https://mcp.example.com" {
		t.Errorf("mcp = %+v", cfg.MCPServers)
	}
	if cfg.Permissions.Mode != "auto" || cfg.Permissions.SessionAllow || !cfg.Permissions.Auto.AllowPush {
		t.Errorf("permissions = %+v", cfg.Permissions)
	}
	if cfg.MaxTurns == nil || *cfg.MaxTurns != 0 || cfg.MaxTokens != 8192 {
		t.Errorf("max_turns/max_tokens = %v/%d", cfg.MaxTurns, cfg.MaxTokens)
	}
	// The partial research override merges per field onto the built-in workflow.
	if w := cfg.Workflows["research"]; w.Skill != "research" || w.Pool.Total != 3 || w.Pool.Concurrent != 5 || len(w.Workers) != 1 {
		t.Errorf("research workflow = %+v", w)
	}
	if cfg.Context.Summarization.Enabled || cfg.Context.Relevance.Heuristic || cfg.Research.RespectRobots {
		t.Errorf("explicit false booleans lost")
	}
	if cfg.Research.Provider != "custom" || cfg.Research.Custom.Headers["X-Client"] != "fuse" {
		t.Errorf("research = %+v", cfg.Research)
	}
}

func TestToConfigSecretRefs(t *testing.T) {
	// Default: placeholders, never a value.
	cfg, err := ToConfig(fullSpec(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.MCPServers[0].Auth.ClientSecret; got != SecretPlaceholder("rentals-token") {
		t.Errorf("client secret = %q", got)
	}
	if got := cfg.Research.Custom.Headers["Authorization"]; got != SecretPlaceholder("search-auth") {
		t.Errorf("auth header = %q", got)
	}

	// A resolver supplies values.
	vals := map[string]string{"rentals-token": "tok-123", "search-auth": "Bearer abc"}
	cfg, err = ToConfig(fullSpec(), func(name string) (string, error) { return vals[name], nil })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MCPServers[0].Auth.ClientSecret != "tok-123" || cfg.Research.Custom.Headers["Authorization"] != "Bearer abc" {
		t.Errorf("resolved secrets not applied")
	}

	// A resolver error names the ref and fails the mapping.
	_, err = ToConfig(fullSpec(), func(name string) (string, error) { return "", errors.New("vault down") })
	if err == nil || !strings.Contains(err.Error(), "vault down") {
		t.Errorf("resolver error = %v", err)
	}
}

func TestToConfigMapsInstructions(t *testing.T) {
	cfg, err := ToConfig(&harnessv1.HarnessSpec{Instructions: tricky}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Instructions != tricky {
		t.Fatalf("instructions = %q, want %q", cfg.Instructions, tricky)
	}
}

func TestFromConfigCarriesInstructions(t *testing.T) {
	cfg := config.Default()
	cfg.Instructions = tricky
	spec, omitted := FromConfig(cfg)
	if len(omitted) != 0 {
		t.Fatalf("unexpected omissions: %+v", omitted)
	}
	if spec.GetInstructions() != tricky {
		t.Fatalf("instructions = %q, want %q", spec.GetInstructions(), tricky)
	}
	cfg.Instructions = ""
	if spec, _ := FromConfig(cfg); spec.GetInstructions() != "" {
		t.Fatalf("empty instructions became %q", spec.GetInstructions())
	}
}

// TestInstructionsLimitMatchesProto pins config.MaxInstructionsChars to the
// string.max_len rule on HarnessSpec.instructions, so the config check and the
// spec validator enforce the same limit.
func TestInstructionsLimitMatchesProto(t *testing.T) {
	fd := (&harnessv1.HarnessSpec{}).ProtoReflect().Descriptor().Fields().ByName("instructions")
	rules := proto.GetExtension(fd.Options(), validate.E_Field).(*validate.FieldRules)
	if got := rules.GetString().GetMaxLen(); got != uint64(config.MaxInstructionsChars) {
		t.Fatalf("proto max_len = %d, config.MaxInstructionsChars = %d", got, config.MaxInstructionsChars)
	}
}

func TestRoundTripSpecConfigSpec(t *testing.T) {
	spec := fullSpec()
	cfg, err := ToConfig(spec, nil)
	if err != nil {
		t.Fatal(err)
	}
	back, omitted := FromConfig(cfg)
	if len(omitted) != 0 {
		t.Fatalf("unexpected omissions: %+v", omitted)
	}
	if !proto.Equal(spec, back) {
		t.Fatalf("round trip mismatch\n got: %v\nwant: %v", back, spec)
	}
}

func TestRoundTripConfigSpecConfig(t *testing.T) {
	cfg, err := config.FromYAML([]byte(`
max_turns: 40
max_tokens: 1000
models:
  default: mine
  mine: {id: cloud/mine, max_tokens: 100, persona: general}
permissions:
  mode: prompt-all
  always_prompt: ["git *"]
  auto: {fetch_deny: ["evil.com"]}
mcp_servers:
  - {name: s1, transport: http, url: "https://s1.example.com", auth: {type: identity, scopes: [x]}, audience: aud}
agents: {max_spawns: 5, tool_timeout_seconds: 9}
workflows:
  research: {pool: {concurrent: 2}}
throughput: {requests_per_minute: 10}
context:
  summarization: {threshold: 0.5}
  relevance: {borderline_hi: 0.9}
pipeline: {synthesis: {max_steps: 7}}
research: {provider: brave, respect_robots: false, custom: {results_path: hits}}
orchestrator: {model: glm}
instructions: |
  You are a code reviewer.
  Flag: correctness first.
`), "fixture")
	if err != nil {
		t.Fatal(err)
	}
	spec, omitted := FromConfig(cfg)
	if len(omitted) != 0 {
		t.Fatalf("unexpected omissions: %+v", omitted)
	}
	back, err := ToConfig(spec, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg, back) {
		t.Fatalf("round trip mismatch\n got: %+v\nwant: %+v", back, cfg)
	}
}

// TestFromConfigOmitsOperatorOnly: every operator-only surface and every inline
// secret is reported, never carried into the spec.
func TestFromConfigOmitsOperatorOnly(t *testing.T) {
	cfg := config.Default()
	cfg.Gateway.URL = "http://gw:4000/v1"
	cfg.SkillPaths = []string{"/opt/skills"}
	cfg.LoopServer.LeaseTTL = "1m"
	cfg.ToolIdentity.SigningKey = "k"
	cfg.Observability.InstanceID = "i-1"
	cfg.Permissions.Auto.WriteRoots = []string{"/tmp"}
	cfg.MCPServers = []config.MCPServerConfig{
		{Name: "local", Transport: "stdio", Command: []string{"npx", "srv"}},
		{Name: "remote", Transport: "http", URL: "https://r.example.com", Auth: config.MCPAuthConfig{Type: "bearer", ClientSecret: "literal-secret", TokenFile: "/x/tok.json"}},
	}
	cfg.Research.Custom.Headers = map[string]string{"X-Api-Key": "abc", "Accept": "application/json"}

	spec, omitted := FromConfig(cfg)
	paths := map[string]bool{}
	for _, o := range omitted {
		paths[o.Path] = true
		if o.Reason == "" {
			t.Errorf("omission %s has no reason", o.Path)
		}
	}
	for _, want := range []string{
		"gateway", "skill_paths", "loop_server", "tool_identity", "observability",
		"permissions.auto.write_roots", "mcp_servers[0]", "mcp_servers[1].auth.client_secret",
		"mcp_servers[1].auth.token_file", "research.custom.headers.X-Api-Key",
	} {
		if !paths[want] {
			t.Errorf("missing omission %q (got %v)", want, paths)
		}
	}
	if len(spec.GetMcpServers()) != 1 || spec.GetMcpServers()[0].GetName() != "remote" || spec.GetMcpServers()[0].GetAuth().GetClientSecretRef() != "" {
		t.Errorf("mcp servers in spec = %v", spec.GetMcpServers())
	}
	h := spec.GetResearch().GetCustom().GetHeaders()
	if _, leaked := h["X-Api-Key"]; leaked || h["Accept"].GetLiteral() != "application/json" {
		t.Errorf("headers in spec = %v", h)
	}
}

func TestSpecPath(t *testing.T) {
	cases := map[string]string{
		"models.default":                      "models.default_alias",
		"max_tokens":                          "max_tokens",
		"context.summarization.threshold":     "context.summarization.threshold",
		"context.relevance.borderline_hi":     "context.relevance.borderline_hi",
		"permissions.auto.classifier_model":   "permissions.auto.classifier_model",
		"orchestrator.model":                  "orchestrator.model",
		"loop_server.lease_ttl":               "",
		"observability":                       "",
		"permissions.auto.write_roots":        "",
		"context.relevance.recency_floor_pct": "context.relevance.recency_floor_pct",
	}
	for in, want := range cases {
		if got := SpecPath(in); got != want {
			t.Errorf("SpecPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestSpecPath_WalksMaps pins that a map field's key renders as ["<key>"],
// the shape protovalidate reports, and that the walk continues into the map
// value's message.
func TestSpecPath_WalksMaps(t *testing.T) {
	cases := map[string]string{
		"workflows.wf.workers.reader.tools":                 `workflows["wf"].workers["reader"].tools`,
		"workflows.wf.pool.total":                           `workflows["wf"].pool.total`,
		"workflows.research.workers.facet-researcher.model": `workflows["research"].workers["facet-researcher"].model`,
		"models.entries.fast.id":                            `models.entries["fast"].id`,
		`workflows.a"b.pool.total`:                          `workflows["a\"b"].pool.total`,
		"workflows.wf":                                      `workflows["wf"]`,
		"workflows.wf.workers":                              `workflows["wf"].workers`,
		"workflows":                                         "workflows",
		"workflows.wf.nope":                                 "",
		"workflows.wf.workers.reader.nope":                  "",
		"workflows.wf.workers.reader.tools.extra":           "",
		"models.default":                                    "models.default_alias",
	}
	for in, want := range cases {
		if got := SpecPath(in); got != want {
			t.Errorf("SpecPath(%q) = %q, want %q", in, got, want)
		}
	}
	// Every map field in HarnessSpec has a message value, so there is no
	// scalar-valued map to prove the walk stops at its key; this check fails
	// if one is added, so the case can be written then.
	var walk func(md protoreflect.MessageDescriptor, seen map[protoreflect.FullName]bool)
	walk = func(md protoreflect.MessageDescriptor, seen map[protoreflect.FullName]bool) {
		if seen[md.FullName()] {
			return
		}
		seen[md.FullName()] = true
		fs := md.Fields()
		for i := 0; i < fs.Len(); i++ {
			fd := fs.Get(i)
			if fd.IsMap() {
				if fd.MapValue().Message() == nil {
					t.Errorf("%s is a scalar-valued map: add a SpecPath case that stops at its key", fd.FullName())
					continue
				}
				walk(fd.MapValue().Message(), seen)
			} else if fd.Message() != nil {
				walk(fd.Message(), seen)
			}
		}
	}
	walk((&harnessv1.HarnessSpec{}).ProtoReflect().Descriptor(), map[protoreflect.FullName]bool{})
}

// TestSpecCannotExpressOperatorSurfaces walks the whole HarnessSpec descriptor
// and fails if any operator-only key exists anywhere in it. This is the trust
// boundary, encoded in the type (ADR-0006, ADR-0060).
func TestSpecCannotExpressOperatorSurfaces(t *testing.T) {
	forbidden := map[string]bool{
		"gateway": true, "loop_server": true, "tool_identity": true, "observability": true,
		"skill_paths": true, "projects": true, "write_roots": true, "command": true,
		"env": true, "token_file": true, "signing_key": true, "issuers": true, "key": true,
		"client_secret": true, "request_timeout": true,
	}
	seen := map[protoreflect.FullName]bool{}
	var walk func(md protoreflect.MessageDescriptor)
	walk = func(md protoreflect.MessageDescriptor) {
		if seen[md.FullName()] {
			return
		}
		seen[md.FullName()] = true
		fields := md.Fields()
		for i := 0; i < fields.Len(); i++ {
			fd := fields.Get(i)
			if forbidden[string(fd.Name())] {
				t.Errorf("%s.%s is an operator-only surface", md.FullName(), fd.Name())
			}
			switch {
			case fd.IsMap():
				if fd.MapValue().Message() != nil {
					walk(fd.MapValue().Message())
				}
			case fd.Message() != nil:
				walk(fd.Message())
			}
		}
	}
	walk((&harnessv1.HarnessSpec{}).ProtoReflect().Descriptor())
}

// TestCredentialHeaderPatternMatchesProto pins credentialHeaderPattern to the
// fuse.headers.credential_literal CEL rule, so FromConfig and the spec
// validator agree on which headers are credentials.
func TestCredentialHeaderPatternMatchesProto(t *testing.T) {
	fd := (&harnessv1.CustomSearchProvider{}).ProtoReflect().Descriptor().Fields().ByName("headers")
	rules := proto.GetExtension(fd.Options(), validate.E_Field).(*validate.FieldRules)
	var expr string
	for _, r := range rules.GetCel() {
		if r.GetId() == "fuse.headers.credential_literal" {
			expr = r.GetExpression()
		}
	}
	if !strings.Contains(expr, "'"+credentialHeaderPattern+"'") {
		t.Fatalf("CEL expression %q does not use credentialHeaderPattern %q", expr, credentialHeaderPattern)
	}
	for name, want := range map[string]bool{
		"Authorization": true, "X-Api-Key": true, "Cookie": true, "x-auth-token": true,
		"Accept": false, "X-Client": false, "Set-Cookie": false,
	} {
		if got := credentialHeader(name); got != want {
			t.Errorf("credentialHeader(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestFromConfigReportsUnrepresentableNumbersAndInertFields(t *testing.T) {
	cfg := config.Default()
	cfg.Throughput.SessionTokens = math.MaxUint32 + 1
	cfg.MCPServers = []config.MCPServerConfig{{
		Name: "r", Transport: "http", URL: "https://r.example.com",
		Command: []string{"x"}, Env: map[string]string{"A": "b"},
	}}
	spec, omitted := FromConfig(cfg)
	paths := map[string]bool{}
	for _, o := range omitted {
		paths[o.Path] = true
	}
	for _, want := range []string{"throughput.session_tokens", "mcp_servers[0].command", "mcp_servers[0].env"} {
		if !paths[want] {
			t.Errorf("missing omission %q (got %v)", want, paths)
		}
	}
	if spec.GetThroughput().GetSessionTokens() != 0 {
		t.Errorf("out-of-range value carried: %d", spec.GetThroughput().GetSessionTokens())
	}
}

func TestFromConfigOmitsModelMetadata(t *testing.T) {
	no := false
	cfg := config.Default()
	cfg.Models.Entries = map[string]config.ModelConfig{
		"fast": {ID: "cloud/fast", Label: "Fast", Description: "d", Tags: []string{"tools"}, Advertise: &no},
	}
	spec, omitted := FromConfig(cfg)
	got := map[string]bool{}
	for _, o := range omitted {
		got[o.Path] = true
	}
	for _, k := range []string{"label", "description", "tags", "advertise"} {
		if !got["models.fast."+k] {
			t.Errorf("missing omission models.fast.%s; got %+v", k, omitted)
		}
	}
	if id := spec.GetModels().GetEntries()["fast"].GetId(); id != "cloud/fast" {
		t.Fatalf("spec id = %q", id)
	}
}
