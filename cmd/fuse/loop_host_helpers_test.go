package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/event/fsstore"
	"github.com/ethanhinson/fuse/internal/observe"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/runtime"
	"github.com/ethanhinson/fuse/internal/skills"
	"github.com/ethanhinson/fuse/internal/tools"
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

// harnessServer builds the production one-shot runtime over a recording gateway, with
// an operator config and two installed skills: the embedded research skill and a user
// skill "notes". One runtime drives one loop, as one-shot does.
func harnessServer(t *testing.T, operatorYAML string) (runtime.Runtime, *recordingGateway) {
	t.Helper()
	gw := &recordingGateway{}
	deps := harnessServerWith(t, operatorYAML, gw)
	return runtime.New(deps), gw
}

// harnessServerWith builds the production one-shot deps over gw, with the same
// operator config and installed skills as harnessServer.
func harnessServerWith(t *testing.T, operatorYAML string, gw http.Handler) runtime.Deps {
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
	reg := registryFromConfig(cfg)
	skillSet, err := skills.LoadWithEmbedded(skills.DefaultDirs())
	if err != nil {
		t.Fatal(err)
	}
	toolReg := loopBaseRegistry(cfg, func() *tools.Registry {
		return defaultToolRegistry(nil, cfg.Research, skillSet.Lookup)
	})
	tree := agent.NewAgentTreeWithConcurrency(reg.Default, reg.Default, cfg.Agents.MaxConcurrent)
	sched := tree.Scheduler()
	sched.SetMaxSpawns(cfg.Agents.MaxSpawns)
	sched.SetQueueBound(cfg.Agents.QueueBound)
	sched.SetSessionTokens(cfg.Throughput.SessionTokens)
	deps, closeMCP := buildOneShotRuntimeDeps(cfg, reg, reg.Default, toolReg, tree, io.Discard, false, nil,
		permissions.AlwaysApprove, rootSystemBlock(cfg, skillSet), false, sessionRateGate(cfg), observe.NoopObserver{})
	t.Cleanup(closeMCP)
	deps.BaseDir = harnessEventDir()
	return deps
}

// harnessEventDir is where a harness loop writes its event log, under the test's HOME.
func harnessEventDir() string {
	return filepath.Join(os.Getenv("HOME"), "harness-events")
}

// loopHistory reads a finished harness loop's event log.
func loopHistory(t *testing.T, loopID string) []event.Event {
	t.Helper()
	fs, err := fsstore.NewFSEventStore(harnessEventDir(), loopID)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	defer fs.Close()
	evs, err := fs.Replay(0)
	if err != nil {
		t.Fatalf("replay event log: %v", err)
	}
	return evs
}

const harnessOperator = `
models:
  default: fast
  fast: {id: cloud/fast}
  house: {id: cloud/house}
max_turns: 2
throughput: {requests_per_minute: 6000}
`

// skillThenDone scripts the gateway: the first reply calls the skill tool for
// name, the second ends the run.
func skillThenDone(name string) []string {
	return []string{
		`{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"skill","arguments":"{\"name\":\"` + name + `\"}"}}]}}]}`,
		`{"choices":[{"message":{"role":"assistant","content":"done"}}]}`,
	}
}

// gatewayToolResults returns the content of every tool-role message in c.
func gatewayToolResults(c gatewayCall) []string {
	var out []string
	for _, m := range c.Messages {
		if m.Role == "tool" {
			out = append(out, m.Content)
		}
	}
	return out
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

// TestOneShotRunsAToolLessConfig (change 0116): on the production one-shot wiring,
// a tool-less config makes its model call with no tools key at all (no
// platform, MCP or internal tools), carries the tool-less persona line, and
// starts no MCP server it names. The same config without no_tools runs the full
// tool set and does reach the MCP server (the positive control).
func TestOneShotRunsAToolLessConfig(t *testing.T) {
	var mcpHits atomic.Int32
	mcpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mcpHits.Add(1)
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}))
	t.Cleanup(mcpSrv.Close)
	base := strings.Replace(harnessOperator, "max_turns: 2", "max_turns: 1", 1) +
		"mcp_servers:\n  - {name: probe, transport: http, url: \"" + mcpSrv.URL + "\"}\n"

	raw := &rawCapture{next: &recordingGateway{}}
	h, err := runtime.New(harnessServerWith(t, base+"no_tools: true\n", raw)).StartLoop(context.Background(), runtime.LoopConfig{Task: "go"})
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	_, _ = h.Wait() // the turn cap ends the run; its error is not the point

	raw.mu.Lock()
	bodies := append([][]byte(nil), raw.bodies...)
	raw.mu.Unlock()
	if len(bodies) != 1 {
		t.Fatalf("tool-less loop made %d model calls, want 1 (max_turns: 1)", len(bodies))
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(bodies[0], &wire); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	for _, key := range []string{"tools", "tool_choice"} {
		if v, ok := wire[key]; ok {
			t.Errorf("tool-less request carries %q on the wire: %s", key, v)
		}
	}
	var c gatewayCall
	if err := json.Unmarshal(bodies[0], &c); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if len(c.Messages) == 0 || c.Messages[0].Role != "system" {
		t.Fatal("first message is not the system prompt")
	}
	sys := c.Messages[0].Content
	if want := agent.ComposeToolLessSystemPrompt(personaOf(t, harnessOperator, "fast"), "", ""); sys != want {
		t.Errorf("tool-less system prompt:\n got: %q\nwant: %q", sys, want)
	}
	for _, banned := range []string{"file and shell tools", "## Skills", "Scratch directory"} {
		if strings.Contains(sys, banned) {
			t.Errorf("tool-less system prompt mentions %q:\n%s", banned, sys)
		}
	}
	if n := mcpHits.Load(); n != 0 {
		t.Errorf("tool-less loop reached its MCP server %d times, want 0", n)
	}

	// Positive control: the same config without no_tools reaches the MCP server
	// and offers the full set, internal tools included.
	ordRaw := &rawCapture{next: &recordingGateway{}}
	h, err = runtime.New(harnessServerWith(t, base, ordRaw)).StartLoop(context.Background(), runtime.LoopConfig{Task: "go"})
	if err != nil {
		t.Fatalf("StartLoop (ordinary): %v", err)
	}
	_, _ = h.Wait()
	if n := mcpHits.Load(); n == 0 {
		t.Error("ordinary loop never reached its MCP server; the tool-less zero-hit check has no control")
	}
	ordRaw.mu.Lock()
	if len(ordRaw.bodies) != 1 {
		n := len(ordRaw.bodies)
		ordRaw.mu.Unlock()
		t.Fatalf("model calls in the ordinary loop = %d, want 1", n)
	}
	var ord gatewayCall
	err = json.Unmarshal(ordRaw.bodies[0], &ord)
	ordRaw.mu.Unlock()
	if err != nil {
		t.Fatalf("decode ordinary request: %v", err)
	}
	have := map[string]bool{}
	for _, n := range ord.toolNames() {
		have[n] = true
	}
	for _, want := range []string{
		"bash", "read_file",
		"spawn_agent",
	} {
		if !have[want] {
			t.Errorf("ordinary loop is not offered %q (offered %v)", want, ord.toolNames())
		}
	}
}

// TestUnreachableMCPServerDoesNotStopTheLoop keeps skip-and-continue: a server that
// cannot connect does not fail the run.
func TestUnreachableMCPServerDoesNotStopTheLoop(t *testing.T) {
	op := harnessOperator + "mcp_servers:\n  - {name: down, transport: http, url: \"http://127.0.0.1:1/mcp\"}\n"
	rt, gw := harnessServer(t, op)
	h, err := rt.StartLoop(context.Background(), runtime.LoopConfig{Task: "go"})
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	_, _ = h.Wait()
	if n := len(gw.snapshot()); n < 1 {
		t.Errorf("the loop made %d gateway requests, want >= 1 (it must continue past the dead server)", n)
	}
}

// TestMCPStaticAndBearerAuthConnect ports the static half of the deleted
// TestEverySpecMCPAuthKindConnects to operator config: on the production one-shot
// wiring, none sends no credential, and bearer and static both send client_secret as a
// bearer token on the MCP handshake, and the server's tool reaches the model.
func TestMCPStaticAndBearerAuthConnect(t *testing.T) {
	for _, tc := range []struct{ typ, auth, want string }{
		{"none", "{type: none}", ""},
		{"bearer", "{type: bearer, client_secret: bearer-tok}", "Bearer bearer-tok"},
		{"static", "{type: static, client_secret: static-tok}", "Bearer static-tok"},
	} {
		t.Run(tc.typ, func(t *testing.T) {
			srv, auths := mcpAuthFakeServer(t)
			op := strings.Replace(harnessOperator, "max_turns: 2", "max_turns: 1", 1) +
				"mcp_servers:\n  - {name: stub, transport: streamable-http, url: \"" + srv.URL + "\", auth: " + tc.auth + "}\n"
			rt, gw := harnessServer(t, op)
			h, err := rt.StartLoop(context.Background(), runtime.LoopConfig{Task: "go"})
			if err != nil {
				t.Fatalf("StartLoop: %v", err)
			}
			_, _ = h.Wait()

			got := auths()
			if len(got) == 0 {
				t.Fatal("the MCP server saw no requests")
			}
			for _, a := range got {
				if a != tc.want {
					t.Errorf("Authorization = %q, want %q (all: %q)", a, tc.want, got)
				}
			}
			calls := gw.snapshot()
			if len(calls) == 0 {
				t.Fatal("no model call")
			}
			if names := strings.Join(calls[0].toolNames(), " "); !strings.Contains(names, "mcp__stub__echo") {
				t.Errorf("model is not offered the server's tool; tools = %s", names)
			}
		})
	}
}
