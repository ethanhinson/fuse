package config

import (
	"bytes"
	"reflect"
	"sort"
	"testing"
)

// operatorBase is a trusted operator config with a value on every axis the
// spec layer treats specially (change 0085).
func operatorBase(t *testing.T) Config {
	t.Helper()
	c, err := FromYAML([]byte(`
gateway: {url: "http://gw", key: "operator-key"}
skill_paths: [/srv/skills]
models:
  default: fast
  fast: {id: cloud/fast}
permissions:
  mode: auto
  disabled: [bash]
  always_prompt: ["git push*"]
  auto:
    classifier_model: fast
    write_roots: [/srv/work]
    fetch_deny: ["*.internal"]
agents: {queue_bound: 2, tool_timeout_seconds: 60}
throughput: {requests_per_minute: 100, tokens_per_minute: 50000, session_tokens: 200000}
pipeline: {synthesis: {max_steps: 10, max_fanout: 4, max_depth: 3, max_attempts: 2}}
workflows:
  research: {skill: research, pool: {concurrent: 4, total: 10, max_depth: 2, tokens: 9000}}
loop_server:
  lease_ttl: 45s
`), "operator")
	if err != nil {
		t.Fatalf("operator config: %v", err)
	}
	return c
}

func overlay(t *testing.T, base Config, doc string) (Config, []string) {
	t.Helper()
	c, refused, err := OverlaySpec(base, []byte(doc), "harness spec")
	if err != nil {
		t.Fatalf("OverlaySpec: %v", err)
	}
	sort.Strings(refused)
	return c, refused
}

func TestOverlaySpecKeepsOperatorValuesWhenOmitted(t *testing.T) {
	base := operatorBase(t)
	got, refused := overlay(t, base, ``)
	if len(refused) != 0 {
		t.Fatalf("refused = %v", refused)
	}
	if !reflect.DeepEqual(got, base) {
		t.Fatalf("an empty spec changed the config:\n got %+v\nwant %+v", got, base)
	}
}

func TestOverlaySpecHonorsClientSettableKeys(t *testing.T) {
	base := operatorBase(t)
	got, refused := overlay(t, base, `
models:
  default: mine
  mine: {id: cloud/mine}
max_turns: 3
orchestrator: {model: mine}
agents: {max_spawns: 5, max_concurrent: 2}
permissions:
  mode: smart
  auto_approve: ["ls*"]
context: {summarization: {threshold: 0.5}}
throughput: {providers: {cloud: {requests_per_minute: 7}}}
`)
	if len(refused) != 0 {
		t.Fatalf("refused = %v", refused)
	}
	if got.Models.Default != "mine" || got.Models.Entries["mine"].ID != "cloud/mine" || got.Models.Entries["fast"].ID != "cloud/fast" {
		t.Errorf("models = %+v", got.Models)
	}
	if got.MaxTurns == nil || *got.MaxTurns != 3 {
		t.Errorf("max_turns = %v", got.MaxTurns)
	}
	if got.Orchestrator.Model != "mine" || got.Agents.MaxSpawns != 5 || got.Agents.MaxConcurrent != 2 {
		t.Errorf("orchestrator/agents = %+v %+v", got.Orchestrator, got.Agents)
	}
	if got.Permissions.Mode != "smart" || !reflect.DeepEqual(got.Permissions.AutoApprove, []string{"ls*"}) {
		t.Errorf("permissions = %+v", got.Permissions)
	}
	if got.Context.Summarization.Threshold != 0.5 {
		t.Errorf("threshold = %v", got.Context.Summarization.Threshold)
	}
	if got.Throughput.Providers["cloud"].RequestsPerMinute != 7 {
		t.Errorf("providers = %+v", got.Throughput.Providers)
	}
	// The base is never mutated by an overlay.
	if base.Models.Default != "fast" || len(base.Models.Entries) != 1 {
		t.Errorf("base mutated: %+v", base.Models)
	}
}

func TestOverlaySpecTightenOnlyAxes(t *testing.T) {
	base := operatorBase(t)
	lowered, refused := overlay(t, base, `
agents: {queue_bound: 1.5, tool_timeout_seconds: 30}
throughput: {requests_per_minute: 10, tokens_per_minute: 1000, session_tokens: 5000}
pipeline: {synthesis: {max_steps: 5, max_fanout: 2, max_depth: 1, max_attempts: 1}}
workflows: {research: {pool: {concurrent: 2, total: 5, max_depth: 1, tokens: 100}}}
`)
	if len(refused) != 0 {
		t.Fatalf("lowering refused: %v", refused)
	}
	if lowered.Agents.QueueBound != 1.5 || lowered.Agents.ToolTimeoutSeconds != 30 ||
		lowered.Throughput.RequestsPerMinute != 10 || lowered.Throughput.TokensPerMinute != 1000 || lowered.Throughput.SessionTokens != 5000 ||
		lowered.Pipeline.Synthesis.MaxSteps != 5 || lowered.Pipeline.Synthesis.MaxAttempts != 1 {
		t.Errorf("lowered values not applied: %+v %+v %+v", lowered.Agents, lowered.Throughput, lowered.Pipeline)
	}
	if p := lowered.Workflows["research"].Pool; p.Concurrent != 2 || p.Total != 5 || p.MaxDepth != 1 || p.Tokens != 100 {
		t.Errorf("workflow pool not lowered: %+v", p)
	}

	raised, refused := overlay(t, base, `
agents: {queue_bound: 3, tool_timeout_seconds: 600}
throughput: {requests_per_minute: 1000, tokens_per_minute: 99999, session_tokens: 999999}
pipeline: {synthesis: {max_steps: 50, max_fanout: 40, max_depth: 30, max_attempts: 20}}
workflows: {research: {pool: {concurrent: 40, tokens: 99999}}}
`)
	want := []string{
		"agents.queue_bound", "agents.tool_timeout_seconds",
		"pipeline.synthesis.max_attempts", "pipeline.synthesis.max_depth", "pipeline.synthesis.max_fanout", "pipeline.synthesis.max_steps",
		"throughput.requests_per_minute", "throughput.session_tokens", "throughput.tokens_per_minute",
		"workflows.research.pool.concurrent", "workflows.research.pool.tokens",
	}
	if !reflect.DeepEqual(refused, want) {
		t.Errorf("refused =\n %v\nwant\n %v", refused, want)
	}
	if !reflect.DeepEqual(raised, base) {
		t.Errorf("a refused loosening changed the config")
	}
}

func TestOverlaySpecIntroducingAnUnsetTightenOnlyAxisIsRefused(t *testing.T) {
	_, refused := overlay(t, Default(), `throughput: {requests_per_minute: 10}`)
	if !reflect.DeepEqual(refused, []string{"throughput.requests_per_minute"}) {
		t.Fatalf("refused = %v", refused)
	}
}

func TestOverlaySpecWorkflows(t *testing.T) {
	base := operatorBase(t)
	got, refused := overlay(t, base, `
workflows:
  research: {skill: deep-research, workers: {reader: {tools: [read_file]}}}
  triage: {skill: triage, pool: {concurrent: 3}}
`)
	if len(refused) != 0 {
		t.Fatalf("refused = %v", refused)
	}
	r := got.Workflows["research"]
	if r.Skill != "deep-research" || len(r.Workers["reader"].Tools) != 1 || r.Pool.Concurrent != 4 {
		t.Errorf("operator workflow = %+v", r)
	}
	if tr := got.Workflows["triage"]; tr.Skill != "triage" || tr.Pool.Concurrent != 3 {
		t.Errorf("client workflow = %+v", tr)
	}
	if _, ok := base.Workflows["triage"]; ok {
		t.Error("base workflows mutated")
	}
}

func TestOverlaySpecPermissionFloorLists(t *testing.T) {
	base := operatorBase(t)
	got, _ := overlay(t, base, `
permissions:
  disabled: [web_fetch, bash]
  always_prompt: ["rm *"]
  auto: {fetch_deny: ["*.corp"], fetch_ask: ["*.example"], classifier_model: fast}
`)
	if !reflect.DeepEqual(got.Permissions.Disabled, []string{"bash", "web_fetch"}) {
		t.Errorf("disabled = %v", got.Permissions.Disabled)
	}
	if !reflect.DeepEqual(got.Permissions.AlwaysPrompt, []string{"git push*", "rm *"}) {
		t.Errorf("always_prompt = %v", got.Permissions.AlwaysPrompt)
	}
	if !reflect.DeepEqual(got.Permissions.Auto.FetchDeny, []string{"*.internal", "*.corp"}) {
		t.Errorf("fetch_deny = %v", got.Permissions.Auto.FetchDeny)
	}
	if !reflect.DeepEqual(got.Permissions.Auto.FetchAsk, []string{"*.example"}) {
		t.Errorf("fetch_ask = %v", got.Permissions.Auto.FetchAsk)
	}
	// write_roots is operator-only: a spec block for permissions.auto never drops it.
	if !reflect.DeepEqual(got.Permissions.Auto.WriteRoots, []string{"/srv/work"}) {
		t.Errorf("write_roots = %v", got.Permissions.Auto.WriteRoots)
	}
}

func TestOverlaySpecIgnoresOperatorOnlyKeys(t *testing.T) {
	base := operatorBase(t)
	var warn bytes.Buffer
	old := warnw
	warnw = &warn
	defer func() { warnw = old }()
	got, refused := overlay(t, base, `
gateway: {url: "http://evil", key: "x", request_timeout: 1m}
skill_paths: [/tmp/evil]
projects: {/: {permissions: {mode: off}}}
loop_server: {lease_ttl: 1s, auth: [{token: t, tenant: x}], secrets: [{name: n, env: HOME}]}
tool_identity: {signing_key: k}
observability: {metrics: {enabled: true}}
permissions: {auto: {write_roots: [/]}}
`)
	if len(refused) != 0 {
		t.Fatalf("refused = %v", refused)
	}
	if !reflect.DeepEqual(got, base) {
		t.Errorf("an operator-only key leaked into the resolved config:\n got %+v\nwant %+v", got, base)
	}
	if warn.Len() != 0 {
		t.Errorf("OverlaySpec wrote a warning: %q", warn.String())
	}
}
