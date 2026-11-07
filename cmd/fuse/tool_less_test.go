package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/observe"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/tools"
)

// Tool-less children (change 0116) at the gateway seam: a tool-less worker is
// offered no tools at every child-builder site, beside a narrowed worker that
// still gets its tools plus the blackboard, and a tool-less role refuses a
// spawn that asks for tools before any child exists.

// TestToolLessRoleRefusesToolsAcrossBindings: a spawn naming a tool-less role
// and a tool set is refused before any node exists, on a local binding and on
// the hosted one. Both go through workflowScope.checkRequest.
func TestToolLessRoleRefusesToolsAcrossBindings(t *testing.T) {
	const refusal = `role "drafter" permits no tools`
	bad := map[string]any{"label": "bad", "task": "BAD-TASK", "role": "drafter", "tools": []string{"read_file"}}

	t.Run("one-shot", func(t *testing.T) {
		gw, tree, toolReg := parityLocalSetup(t, roleOperatorYAML, roleChildRoute, nil, oneShotParityBuild)
		before := len(tree.SnapshotAll())
		res := toolReg.Execute(context.Background(), "spawn_agent", argsJSON(bad))
		if !res.IsError || !strings.Contains(res.Output, refusal) {
			t.Fatalf("spawn_agent = %+v, want an error containing %q", res, refusal)
		}
		if n := len(tree.SnapshotAll()); n != before {
			t.Errorf("refused spawn created %d nodes, want 0", n-before)
		}
		if n := len(gw.callsFor("BAD-TASK")); n != 0 {
			t.Errorf("BAD-TASK requests = %d, want 0", n)
		}
	})

	t.Run("loop-server/operator", func(t *testing.T) {
		r := runHostedConfig(t, hostedOperatorYAML+roleParityRoles, spawnRoute([]map[string]any{bad}))
		if got := countContaining(toolResults(turnRequest(t, r.gw, "ROOT-TASK", 1)), refusal); got != 1 {
			t.Fatalf("root turn-1 tool results carrying %q = %d, want 1", refusal, got)
		}
		if n := len(r.gw.callsFor("BAD-TASK")); n != 0 {
			t.Errorf("BAD-TASK requests = %d, want 0", n)
		}
		for _, v := range r.tree.SnapshotAll() {
			if v.Label == "bad" {
				t.Errorf("refused spawn created node %q", v.ID)
			}
		}
	})
}

// TestOneShotNoToolsSendsNoTools (change 0116): `fuse "<task>"` with
// no_tools: true in config makes its first model call with no tools key, the
// tool-less persona line, and starts no configured MCP server.
func TestOneShotNoToolsSendsNoTools(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var mcpHits atomic.Int32
	mcpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mcpHits.Add(1)
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}))
	t.Cleanup(mcpSrv.Close)

	dir := filepath.Join(os.Getenv("HOME"), ".fuse")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "no_tools: true\n" +
		"mcp_servers:\n" +
		"  - {name: probe, transport: http, url: \"" + mcpSrv.URL + "\"}\n"
	if err := os.WriteFile(filepath.Join(dir, "config.yml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var bodies [][]byte
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, b)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"DONE"}}]}`)
	}))
	t.Cleanup(gw.Close)
	t.Setenv("LLM_GATEWAY_URL", gw.URL)
	t.Setenv("LLM_GATEWAY_KEY", "tkn")

	var out, errb bytes.Buffer
	if code := run([]string{"--model", "deepseek-flash", "say hi"}, &out, &errb); code != 0 {
		t.Fatalf("run exit=%d stderr=%s", code, errb.String())
	}

	mu.Lock()
	got := append([][]byte(nil), bodies...)
	mu.Unlock()
	if len(got) == 0 {
		t.Fatal("no model call recorded")
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(got[0], &wire); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	for _, key := range []string{"tools", "tool_choice"} {
		if v, ok := wire[key]; ok {
			t.Errorf("tool-less one-shot request carries %q on the wire: %s", key, v)
		}
	}
	var c gatewayCall
	if err := json.Unmarshal(got[0], &c); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if len(c.Messages) == 0 || c.Messages[0].Role != "system" {
		t.Fatal("first message is not the system prompt")
	}
	// The run loaded its config from the same HOME; resolve the alias the same way.
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	mc, err := registryFromConfig(cfg).Resolve("deepseek-flash")
	if err != nil {
		t.Fatal(err)
	}
	sys := c.Messages[0].Content
	if want := agent.ComposeToolLessSystemPrompt(mc.Persona, mc.SystemPrefix, cfg.Instructions); sys != want {
		t.Errorf("tool-less one-shot system prompt:\n got: %q\nwant: %q", sys, want)
	}
	if !strings.Contains(sys, agent.ToolLessPersonaPrompt(mc.Persona)) || strings.Contains(sys, "file and shell tools") {
		t.Errorf("tool-less one-shot system prompt is not the tool-less persona line:\n%s", sys)
	}
	if n := mcpHits.Load(); n != 0 {
		t.Errorf("tool-less one-shot reached its configured MCP server %d times, want 0", n)
	}

	// Positive control: the same MCP config without no_tools does reach the server.
	if err := os.WriteFile(filepath.Join(dir, "config.yml"), []byte(strings.TrimPrefix(body, "no_tools: true\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errb.Reset()
	_ = run([]string{"--model", "deepseek-flash", "say hi"}, &out, &errb) // the failing MCP server may fail the run; the hit is the point
	if n := mcpHits.Load(); n == 0 {
		t.Errorf("ordinary one-shot never reached its configured MCP server; the tool-less zero-hit check has no control (stderr=%s)", errb.String())
	}
}

// TestLoopBaseRegistryToolLess: a tool-less config gets an empty registry and
// never builds the ordinary one; any other config gets exactly build().
func TestLoopBaseRegistryToolLess(t *testing.T) {
	built := false
	build := func() *tools.Registry {
		built = true
		return defaultToolRegistry(nil, config.Default().Research, nil)
	}
	reg := loopBaseRegistry(config.Config{NoTools: true}, build)
	if n := len(reg.Tools()); n != 0 {
		t.Errorf("tool-less base registry has %d tools, want 0", n)
	}
	if built {
		t.Error("tool-less base registry ran build")
	}

	want := tools.NewRegistry()
	if got := loopBaseRegistry(config.Config{}, func() *tools.Registry { return want }); got != want {
		t.Error("ordinary base registry is not build()'s registry")
	}
}

// TestLoopMCPServersToolLess: a tool-less config starts no MCP server.
func TestLoopMCPServersToolLess(t *testing.T) {
	servers := []config.MCPServerConfig{{Name: "probe"}}
	if got := loopMCPServers(config.Config{NoTools: true, MCPServers: servers}); got != nil {
		t.Errorf("tool-less MCP servers = %v, want nil", got)
	}
	if got := loopMCPServers(config.Config{MCPServers: servers}); !reflect.DeepEqual(got, servers) {
		t.Errorf("ordinary MCP servers = %v, want %v", got, servers)
	}
}

// TestWithoutNoTools: an entry point that always runs with its tools clears
// no_tools and says so once; an ordinary config passes through silently.
func TestWithoutNoTools(t *testing.T) {
	var buf bytes.Buffer
	got := withoutNoTools(config.Config{NoTools: true}, &buf, "shell")
	if got.NoTools {
		t.Error("withoutNoTools left NoTools set")
	}
	if lines := strings.Count(buf.String(), "\n"); lines != 1 || !strings.Contains(buf.String(), "shell") {
		t.Errorf("warning = %q, want one line naming shell", buf.String())
	}

	buf.Reset()
	if got := withoutNoTools(config.Config{}, &buf, "shell"); got.NoTools {
		t.Error("withoutNoTools set NoTools")
	}
	if buf.Len() != 0 {
		t.Errorf("ordinary config wrote %q, want nothing", buf.String())
	}
}

// TestRootToolsToolLess: a tool-less loop's root gets no spawn_agent,
// blackboard or pipeline_run.
func TestRootToolsToolLess(t *testing.T) {
	h := newWiringHarness(t)
	h.w.cfg.NoTools = true
	reg := tools.NewRegistry()
	h.w.rootTools(reg, h.root)
	if n := len(reg.Tools()); n != 0 {
		var names []string
		for _, tl := range reg.Tools() {
			names = append(names, tl.Name())
		}
		t.Errorf("tool-less root tools = %v, want none", names)
	}
}

// TestResearchProbeIgnoresNoTools (change 0116): the research probe always
// runs with its tools, so no_tools in config is cleared with one warning and
// the probe's first model call still offers its tools.
func TestResearchProbeIgnoresNoTools(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := filepath.Join(os.Getenv("HOME"), ".fuse")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yml"), []byte("no_tools: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var calls []gatewayCall
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var c gatewayCall
		_ = json.NewDecoder(r.Body).Decode(&c)
		mu.Lock()
		calls = append(calls, c)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"DONE"}}]}`)
	}))
	t.Cleanup(gw.Close)
	t.Setenv("LLM_GATEWAY_URL", gw.URL)
	t.Setenv("LLM_GATEWAY_KEY", "tkn")

	var out, errb bytes.Buffer
	if code := run([]string{"research-probe", "--model", "deepseek-flash", "--timeout", "30s", "what is fuse"}, &out, &errb); code != 0 {
		t.Fatalf("run exit=%d stderr=%s", code, errb.String())
	}
	if n := strings.Count(errb.String(), "no_tools applies to one-shot runs"); n != 1 {
		t.Errorf("no_tools warnings on stderr = %d, want 1:\n%s", n, errb.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) == 0 {
		t.Fatal("no model call recorded")
	}
	if len(calls[0].Tools) == 0 {
		t.Error("research probe under no_tools was offered no tools, want its full set")
	}
	if len(calls[0].Messages) > 0 && strings.Contains(calls[0].Messages[0].Content, "You have no tools") {
		t.Errorf("research probe composed the tool-less prompt:\n%s", calls[0].Messages[0].Content)
	}
}

// TestNoToolsRejectsCLIModel: a cli/ model runs through a subprocess that
// always attaches fuse's own tools, so a tool-less config on one fails closed
// at build time, on the root and the child path alike, naming the model. An
// ordinary cli/ build is unaffected.
func TestNoToolsRejectsCLIModel(t *testing.T) {
	t.Setenv("FUSE_HITL_MODE", "")
	reg := model.NewRegistry("c", map[string]model.ModelConfig{"c": {ID: "cli/claude"}})
	build := map[string]func(cfg config.Config) error{
		"buildAgentCore": func(cfg config.Config) error {
			_, _, err := buildAgentCore(cfg, reg, "c", discardRenderer{}, "", "", nil, "root", tools.NewRegistry(), permissions.AlwaysApprove, nil, false, nil, nil, observe.NoopObserver{})
			return err
		},
		"buildChildAgent": func(cfg config.Config) error {
			_, err := buildChildAgent(cfg, reg, "c", discardRenderer{}, "", tools.NewRegistry(), permissions.AlwaysApprove, nil, "child", nil, false, nil, nil)
			return err
		},
	}
	for name, fn := range build {
		t.Run(name, func(t *testing.T) {
			cfg := config.Default()
			cfg.NoTools = true
			err := fn(cfg)
			if err == nil || !strings.Contains(err.Error(), "no_tools") || !strings.Contains(err.Error(), `"cli/claude"`) {
				t.Fatalf("tool-less cli build error = %v, want a no_tools error naming \"cli/claude\"", err)
			}
			cfg.NoTools = false
			if err := fn(cfg); err != nil {
				t.Fatalf("ordinary cli build: %v", err)
			}
		})
	}
}
