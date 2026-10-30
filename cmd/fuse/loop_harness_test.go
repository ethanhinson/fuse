package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ethanhinson/fuse/harness"
	harnessv1 "github.com/ethanhinson/fuse/harness/v1"
	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/observe"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/runtime"
	"github.com/ethanhinson/fuse/internal/skills"
)

// gatewayCall is one chat-completions request the scripted gateway received.
type gatewayCall struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
}

// recordingGateway answers every request with a tool call (so a loop only stops
// at its turn cap) and records what it was asked.
type recordingGateway struct {
	mu    sync.Mutex
	calls []gatewayCall
}

func (g *recordingGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var c gatewayCall
	_ = json.Unmarshal(body, &c)
	g.mu.Lock()
	g.calls = append(g.calls, c)
	g.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"skill","arguments":"{\"name\":\"research\"}"}}]}}]}`)
}

func (g *recordingGateway) snapshot() []gatewayCall {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]gatewayCall(nil), g.calls...)
}

// harnessServer builds the production loop-server runtime (the deps
// loop-serve-net uses) over a scripted gateway, with an operator config and two
// installed skills: the embedded research skill and a user skill "notes".
func harnessServer(t *testing.T, operatorYAML string) (runtime.Runtime, *recordingGateway, *loopServerHost) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	notes := filepath.Join(home, ".fuse", "skills", "notes")
	if err := os.MkdirAll(notes, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(notes, "SKILL.md"), []byte("---\nname: notes\ndescription: take notes\n---\nTake notes.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gw := &recordingGateway{}
	srv := httptest.NewServer(gw)
	t.Cleanup(srv.Close)

	cfg, err := config.FromYAML([]byte(operatorYAML), "operator")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Gateway.URL, cfg.Gateway.Key = srv.URL, "tkn"
	reg := registryFromConfig(cfg)
	skillSet, err := skills.LoadWithEmbedded(skills.DefaultDirs())
	if err != nil {
		t.Fatal(err)
	}
	toolReg := defaultToolRegistry(nil, cfg.Research, skillSet.Lookup)
	deps := assembleLoopServerRuntimeDeps(nil, cfg, reg, reg.Default, toolReg, rootSystemBlock(cfg, skillSet),
		permissions.AlwaysApprove, sessionRateGate(cfg), observe.NoopObserver{}, nil, nil, skillSet)
	deps.BaseDir = t.TempDir()
	return runtime.New(deps), gw, deps.Harness.(*loopServerHost)
}

const harnessOperator = `
models:
  default: fast
  fast: {id: cloud/fast}
  house: {id: cloud/house}
max_turns: 2
throughput: {requests_per_minute: 6000}
`

// TestLoopServerRunsALoopFromItsHarnessSpec is the composition-root proof for
// change 0085: on the production loop-server wiring, a loop started with a spec
// runs the spec's model alias, turn cap and skill subset, while a loop started
// without one keeps the server's.
func TestLoopServerRunsALoopFromItsHarnessSpec(t *testing.T) {
	rt, gw, _ := harnessServer(t, harnessOperator)
	turns := uint32(1)
	spec := &harnessv1.HarnessSpec{
		Models:   &harnessv1.Models{DefaultAlias: "house"},
		MaxTurns: &turns,
		Skills:   []*harnessv1.SkillRef{{Name: "research"}},
	}
	h, err := rt.StartLoop(context.Background(), runtime.LoopConfig{Task: "go", Harness: spec})
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	_, _ = h.Wait() // the turn cap ends the run; its error is not the point
	calls := gw.snapshot()
	if len(calls) != 1 {
		t.Fatalf("spec loop made %d model calls, want 1 (max_turns: 1)", len(calls))
	}
	if calls[0].Model != "cloud/house" {
		t.Errorf("spec loop model = %q, want cloud/house", calls[0].Model)
	}
	sys := calls[0].Messages[0].Content
	if !strings.Contains(sys, "- research") || strings.Contains(sys, "- notes") {
		t.Errorf("spec loop skills listing is not exactly [research]:\n%s", sys)
	}

	// The same server, no spec: the operator's model, turn cap and full skill set.
	h, err = rt.StartLoop(context.Background(), runtime.LoopConfig{Task: "go"})
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	_, _ = h.Wait()
	calls = gw.snapshot()[1:]
	if len(calls) != 2 || calls[0].Model != "cloud/fast" {
		t.Fatalf("server loop made %d calls on %q, want 2 on cloud/fast", len(calls), calls[0].Model)
	}
	if sys := calls[0].Messages[0].Content; !strings.Contains(sys, "- research") || !strings.Contains(sys, "- notes") {
		t.Errorf("server loop does not list every installed skill:\n%s", sys)
	}
}

func TestLoopServerRefusesAnInvalidHarnessSpec(t *testing.T) {
	rt, gw, _ := harnessServer(t, harnessOperator)
	spec := &harnessv1.HarnessSpec{
		Skills:     []*harnessv1.SkillRef{{Name: "missing"}},
		Throughput: &harnessv1.Throughput{RequestsPerMinute: 9000},
		McpServers: []*harnessv1.McpServer{{
			Name: "s", Transport: harnessv1.McpTransport_MCP_TRANSPORT_HTTP, Url: "https://mcp.example.com",
			Auth: &harnessv1.McpAuth{Type: harnessv1.McpAuthType_MCP_AUTH_TYPE_BEARER, ClientSecretRef: "tok"},
		}},
	}
	_, err := rt.StartLoop(context.Background(), runtime.LoopConfig{Task: "go", Harness: spec})
	var ve *harness.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want a ValidationError", err)
	}
	var paths []string
	for _, v := range ve.Violations {
		paths = append(paths, v.FieldPath+" "+v.RuleID)
	}
	want := []string{
		"mcp_servers[0].auth.client_secret_ref fuse.secret_ref",
		"skills[0].name fuse.skill_ref",
		"throughput.requests_per_minute fuse.tighten_only",
	}
	if strings.Join(paths, "|") != strings.Join(want, "|") {
		t.Fatalf("violations = %v, want %v", paths, want)
	}
	if n := len(gw.snapshot()); n != 0 {
		t.Fatalf("a refused spec reached the model %d times", n)
	}
}

func TestLoopServerResolvesOperatorSecretsForTheTenant(t *testing.T) {
	t.Setenv("FUSE_TEST_MCP_TOKEN", "operator-held-token")
	_, _, host := harnessServer(t, harnessOperator+`
loop_server:
  secrets:
    - {name: tok, env: FUSE_TEST_MCP_TOKEN, tenants: [acme]}
`)
	spec := &harnessv1.HarnessSpec{McpServers: []*harnessv1.McpServer{{
		Name: "s", Transport: harnessv1.McpTransport_MCP_TRANSPORT_HTTP, Url: "https://mcp.example.com",
		Auth: &harnessv1.McpAuth{Type: harnessv1.McpAuthType_MCP_AUTH_TYPE_BEARER, ClientSecretRef: "tok"},
	}}}
	if _, err := host.ResolveHarness(context.Background(), "other", spec); err == nil {
		t.Fatal("another tenant resolved the secret")
	}
	f, err := host.ResolveHarness(context.Background(), "acme", spec)
	if err != nil || f.BuildAgent == nil {
		t.Fatalf("ResolveHarness(acme) = %v", err)
	}
}

func TestLoopRateGateChainsOnlyWhenTheLoopDiffers(t *testing.T) {
	server := config.Default()
	server.Throughput.RequestsPerMinute = 600
	serverGate := sessionRateGate(server)
	if g := loopRateGate(server, serverGate, server); g != serverGate {
		t.Fatal("an unchanged throughput did not reuse the server gate")
	}
	tighter := server
	tighter.Throughput.RequestsPerMinute = 60
	g, ok := loopRateGate(server, serverGate, tighter).(chainedRateGate)
	if !ok || len(g) != 2 || g[1] != serverGate {
		t.Fatalf("a tighter loop gate = %#v, want own bucket then the server gate", g)
	}
	if got := loopRateGate(config.Default(), nil, tighter); got == nil {
		t.Fatal("a loop limit on an unlimited server was dropped")
	}
	var _ model.RateGate = chainedRateGate{}
}
