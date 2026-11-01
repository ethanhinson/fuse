package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/harnessresolve"
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
	Tools []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tools"`
}

func (c gatewayCall) toolNames() []string {
	var out []string
	for _, t := range c.Tools {
		out = append(out, t.Function.Name)
	}
	return out
}

// recordingGateway answers every request with a tool call (so a loop only stops
// at its turn cap) and records what it was asked.
type recordingGateway struct {
	mu    sync.Mutex
	calls []gatewayCall
	// script, when set, replaces the default tool-call reply: request i gets
	// script[i] (the last entry repeats). Each entry is a full response body.
	script []string
}

func (g *recordingGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var c gatewayCall
	_ = json.Unmarshal(body, &c)
	g.mu.Lock()
	idx := len(g.calls)
	g.calls = append(g.calls, c)
	script := g.script
	g.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if len(script) > 0 {
		if idx >= len(script) {
			idx = len(script) - 1
		}
		io.WriteString(w, script[idx])
		return
	}
	io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"skill","arguments":"{\"name\":\"research\"}"}}]}}]}`)
}

func (g *recordingGateway) snapshot() []gatewayCall {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]gatewayCall(nil), g.calls...)
}

// harnessServer builds the production loop-server runtime (the deps
// loop-serve-net uses) over a recording gateway, with an operator config and two
// installed skills: the embedded research skill and a user skill "notes".
func harnessServer(t *testing.T, operatorYAML string) (runtime.Runtime, *recordingGateway, *loopServerHost) {
	t.Helper()
	gw := &recordingGateway{}
	deps, host := harnessServerWith(t, operatorYAML, gw)
	return runtime.New(deps), gw, host
}

// harnessServerWith builds the production loop-server deps over gw, with the
// same operator config and installed skills as harnessServer.
func harnessServerWith(t *testing.T, operatorYAML string, gw http.Handler) (runtime.Deps, *loopServerHost) {
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
	srv := httptest.NewServer(gw)
	t.Cleanup(srv.Close)

	cfg, err := config.FromYAML([]byte(operatorYAML), "operator")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Gateway.URL, cfg.Gateway.Key = srv.URL, "tkn"
	// These tests point specs at loopback httptest servers and unresolvable
	// example hosts; the spec policy has its own tests (change 0101).
	noDeny := false
	cfg.LoopServer.SpecPolicy = config.SpecPolicyConfig{DenyPrivateNetworks: &noDeny, AllowHTTPHosts: []string{"127.0.0.1"}}
	reg := registryFromConfig(cfg)
	skillSet, err := skills.LoadWithEmbedded(skills.DefaultDirs())
	if err != nil {
		t.Fatal(err)
	}
	toolReg := defaultToolRegistry(nil, cfg.Research, skillSet.Lookup)
	deps := assembleLoopServerRuntimeDeps(nil, cfg, reg, reg.Default, toolReg, rootSystemBlock(cfg, skillSet),
		permissions.AlwaysApprove, sessionRateGate(cfg), observe.NoopObserver{}, nil, nil, skillSet)
	deps.BaseDir = t.TempDir()
	return deps, deps.Harness.(*loopServerHost)
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
	// The value reaches exactly the loop's own MCP config, nowhere else.
	res, err := harnessresolve.Resolver{
		Base:    host.operator.cfg,
		Secrets: harnessresolve.ConfigSecrets(host.operator.cfg.LoopServer.Secrets),
	}.Resolve(context.Background(), "acme", spec)
	if err != nil {
		t.Fatal(err)
	}
	if got := host.settingsFor(res).cfg.MCPServers[0].Auth.ClientSecret; got != "operator-held-token" {
		t.Fatalf("loop MCP secret = %q", got)
	}
	if len(host.operator.cfg.MCPServers) != 0 {
		t.Fatal("the operator config gained the spec's MCP server")
	}
}

// TestLoopHarnessRecordNeverHoldsASecretValue runs a spec loop whose MCP server
// takes an operator secret and checks the durable loop.harness record carries
// the secret's name and not its value.
func TestLoopHarnessRecordNeverHoldsASecretValue(t *testing.T) {
	t.Setenv("FUSE_TEST_MCP_TOKEN", "operator-held-token")
	rt, _, _ := harnessServer(t, harnessOperator+`
loop_server:
  secrets:
    - {name: tok, env: FUSE_TEST_MCP_TOKEN}
`)
	turns := uint32(1)
	spec := &harnessv1.HarnessSpec{MaxTurns: &turns, McpServers: []*harnessv1.McpServer{{
		Name: "s", Transport: harnessv1.McpTransport_MCP_TRANSPORT_HTTP, Url: "http://127.0.0.1:1/mcp",
		Auth: &harnessv1.McpAuth{Type: harnessv1.McpAuthType_MCP_AUTH_TYPE_BEARER, ClientSecretRef: "tok"},
	}}}
	h, err := rt.StartLoop(context.Background(), runtime.LoopConfig{Task: "go", Harness: spec})
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	_, _ = h.Wait()
	hist, err := rt.Attach(context.Background(), "", h.ID(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) == 0 || hist[0].Kind != event.KindLoopHarness {
		t.Fatalf("first event is not loop.harness: %v", hist)
	}
	if !strings.Contains(string(hist[0].Payload), `"tok"`) {
		t.Errorf("record lost the secret name: %s", hist[0].Payload)
	}
	for _, e := range hist {
		if strings.Contains(string(e.Payload), "operator-held-token") {
			t.Fatalf("a %s event holds the secret value", e.Kind)
		}
	}
}

// TestSpecOAuthServerNeverTouchesTheHostTokenCache guards the token cache the
// operator's own oauth2 MCP servers use (~/.fuse/mcp-tokens/<name>.json): a
// spec naming a server after one of them is refused before anything dials, so
// the cached token is never read, sent, refreshed or overwritten.
func TestSpecOAuthServerNeverTouchesTheHostTokenCache(t *testing.T) {
	rt, gw, _ := harnessServer(t, harnessOperator)
	home := os.Getenv("HOME")
	tokDir := filepath.Join(home, ".fuse", "mcp-tokens")
	if err := os.MkdirAll(tokDir, 0o700); err != nil {
		t.Fatal(err)
	}
	tokFile := filepath.Join(tokDir, "github.json")
	cached := []byte(`{"access_token":"operator-cached","refresh_token":"r"}`)
	if err := os.WriteFile(tokFile, cached, 0o600); err != nil {
		t.Fatal(err)
	}
	var hits int
	var mu sync.Mutex
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
	}))
	defer attacker.Close()
	spec := &harnessv1.HarnessSpec{McpServers: []*harnessv1.McpServer{{
		Name: "github", Transport: harnessv1.McpTransport_MCP_TRANSPORT_HTTP, Url: attacker.URL,
		Auth: &harnessv1.McpAuth{Type: harnessv1.McpAuthType_MCP_AUTH_TYPE_OAUTH2},
	}}}
	_, err := rt.StartLoop(context.Background(), runtime.LoopConfig{Task: "go", Harness: spec})
	var ve *harness.ValidationError
	if !errors.As(err, &ve) || ve.Violations[0].FieldPath != "mcp_servers[0].auth.type" || ve.Violations[0].RuleID != harness.RuleOperatorOnly {
		t.Fatalf("err = %v, want an operator_only violation", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != 0 || len(gw.snapshot()) != 0 {
		t.Fatalf("the refused spec reached a server: attacker=%d gateway=%d", hits, len(gw.snapshot()))
	}
	if got, _ := os.ReadFile(tokFile); string(got) != string(cached) {
		t.Fatal("the host token cache changed")
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

// mcpAuthFakeServer is a minimal streamable-HTTP MCP server exposing one tool
// "echo". It records the Authorization header of each request.
func mcpAuthFakeServer(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var auths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		var req struct {
			Method string          `json:"method"`
			ID     json.RawMessage `json:"id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		reply := func(result string) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":%s}`, req.ID, result)
		}
		switch req.Method {
		case "initialize":
			reply(`{"protocolVersion":"2025-03-26","capabilities":{}}`)
		case "tools/list":
			reply(`{"tools":[{"name":"echo","description":"echo","inputSchema":{"type":"object"}}]}`)
		default:
			// Intentional: notifications (e.g. notifications/initialized) and any
			// other unknown method get a bodyless 202, as the spec allows.
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), auths...) }
}

func mcpServerEvents(t *testing.T, hist []event.Event) []event.MCPServerPayload {
	t.Helper()
	var out []event.MCPServerPayload
	for _, e := range hist {
		if e.Kind != event.KindMCPServer {
			continue
		}
		var p event.MCPServerPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

// TestEverySpecMCPAuthKindConnects is the golden for ADR-0062: every MCP auth
// kind the server advertises to a spec must actually connect, send its
// credential, and list tools, and the loop must report it as an mcp.server event.
func TestEverySpecMCPAuthKindConnects(t *testing.T) {
	t.Setenv("FUSE_TEST_MCP_TOKEN", "operator-held-token")
	for _, kind := range harnessresolve.SpecMCPAuthKinds() {
		t.Run(kind.String(), func(t *testing.T) {
			rt, gw, _ := harnessServer(t, harnessOperator+`
loop_server:
  secrets:
    - {name: tok, env: FUSE_TEST_MCP_TOKEN}
`)
			srv, auths := mcpAuthFakeServer(t)
			auth := &harnessv1.McpAuth{Type: kind}
			wantAuth := ""
			if kind != harnessv1.McpAuthType_MCP_AUTH_TYPE_UNSPECIFIED {
				auth.ClientSecretRef = "tok"
				wantAuth = "Bearer operator-held-token"
			}
			turns := uint32(1)
			spec := &harnessv1.HarnessSpec{MaxTurns: &turns, McpServers: []*harnessv1.McpServer{{
				Name: "s", Transport: harnessv1.McpTransport_MCP_TRANSPORT_STREAMABLE_HTTP, Url: srv.URL, Auth: auth,
			}}}
			h, err := rt.StartLoop(context.Background(), runtime.LoopConfig{Task: "go", Harness: spec})
			if err != nil {
				t.Fatalf("StartLoop: %v", err)
			}
			_, _ = h.Wait()
			hist, err := rt.Attach(context.Background(), "", h.ID(), 0)
			if err != nil {
				t.Fatal(err)
			}
			evs := mcpServerEvents(t, hist)
			if len(evs) != 1 {
				t.Fatalf("mcp.server events = %+v", evs)
			}
			if hist[1].Kind != event.KindMCPServer {
				t.Errorf("second event = %s, want mcp.server right after loop.harness", hist[1].Kind)
			}
			if !evs[0].Connected || evs[0].Error != "" || len(evs[0].Tools) != 1 || evs[0].Tools[0] != "mcp:s/echo" {
				t.Fatalf("server did not connect and list mcp:s/echo: %+v", evs[0])
			}
			calls := gw.snapshot()
			if len(calls) == 0 {
				t.Fatal("the gateway received no request")
			}
			found := false
			for _, n := range calls[0].toolNames() {
				found = found || n == "mcp:s/echo"
			}
			if !found {
				t.Errorf("first gateway request tools = %v, want to include mcp:s/echo", calls[0].toolNames())
			}
			if evs[0].Name != "s" {
				t.Errorf("name = %q", evs[0].Name)
			}
			if wantAuth == "" {
				for _, a := range auths() {
					if a != "" {
						t.Errorf("unauthenticated kind sent an Authorization header: %q", a)
					}
				}
			} else {
				seen := false
				for _, a := range auths() {
					seen = seen || a == wantAuth
				}
				if !seen {
					t.Errorf("no request carried %q; saw %v", wantAuth, auths())
				}
			}
			for _, e := range hist {
				if e.Kind == event.KindMCPServer && strings.Contains(string(e.Payload), "operator-held-token") {
					t.Fatal("mcp.server event holds the secret value")
				}
			}
		})
	}
}

// TestUnreachableMCPServerIsReportedAndLoopContinues keeps skip-and-continue: a
// server that cannot connect is reported connected=false and does not fail the loop.
func TestUnreachableMCPServerIsReportedAndLoopContinues(t *testing.T) {
	rt, gw, _ := harnessServer(t, harnessOperator)
	turns := uint32(1)
	spec := &harnessv1.HarnessSpec{MaxTurns: &turns, McpServers: []*harnessv1.McpServer{{
		Name: "down", Transport: harnessv1.McpTransport_MCP_TRANSPORT_HTTP, Url: "http://127.0.0.1:1/mcp",
	}}}
	h, err := rt.StartLoop(context.Background(), runtime.LoopConfig{Task: "go", Harness: spec})
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	_, _ = h.Wait()
	hist, err := rt.Attach(context.Background(), "", h.ID(), 0)
	if err != nil {
		t.Fatal(err)
	}
	evs := mcpServerEvents(t, hist)
	if len(evs) != 1 || evs[0].Connected || evs[0].Error == "" {
		t.Fatalf("want one disconnected mcp.server event with an error, got %+v", evs)
	}
	if hist[0].Kind != event.KindLoopHarness {
		t.Errorf("first event = %s", hist[0].Kind)
	}
	if hist[1].Kind != event.KindMCPServer {
		t.Errorf("second event = %s, want mcp.server", hist[1].Kind)
	}
	if n := len(gw.snapshot()); n < 1 {
		t.Errorf("the loop made %d gateway requests, want >= 1 (it must continue past the dead server)", n)
	}
}

func TestRedactMCPError(t *testing.T) {
	cases := map[string]string{
		`Post "https://user:pw@host.example/mcp?token=abc&x=1": dial tcp: refused`: `Post "https://host.example/mcp": dial tcp: refused`,
		`get http://h/p?api_key=s3cret failed.`:                                    `get http://h/p failed.`,
		`no url here`:                                                              `no url here`,
	}
	for in, want := range cases {
		got := redactMCPError(in)
		if got != want {
			t.Errorf("redactMCPError(%q) = %q, want %q", in, got, want)
		}
		if strings.Contains(got, "pw@") || strings.Contains(got, "s3cret") || strings.Contains(got, "token=") {
			t.Errorf("secret survived: %q", got)
		}
	}
}
