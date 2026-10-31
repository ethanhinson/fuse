package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/probe"
	"github.com/ethanhinson/fuse/internal/tools"
)

// Acceptance for per-spawn workflow activation (change 0091) on the local
// bindings: the real deps builders, driven through the binding's own root
// spawn_agent tool, against a scripted gateway. The helpers below are shared
// with the hosted acceptance tests.

// routedGateway answers each chat request by routing on its first user message
// and how many assistant turns it already carries, and records every request.
type routedGateway struct {
	mu    sync.Mutex
	calls []gatewayCall
	route func(task string, turn int, c gatewayCall) string
}

func (g *routedGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var c gatewayCall
	_ = json.Unmarshal(body, &c)
	g.mu.Lock()
	g.calls = append(g.calls, c)
	g.mu.Unlock()
	task, turn := "", 0
	for _, m := range c.Messages {
		if m.Role == "user" && task == "" {
			task = m.Content
		}
		if m.Role == "assistant" {
			turn++
		}
	}
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, g.route(task, turn, c))
}

// callsFor returns the recorded requests whose first user message contains marker.
func (g *routedGateway) callsFor(marker string) []gatewayCall {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []gatewayCall
	for _, c := range g.calls {
		for _, m := range c.Messages {
			if m.Role == "user" {
				if strings.Contains(m.Content, marker) {
					out = append(out, c)
				}
				break
			}
		}
	}
	return out
}

// call is one scripted tool call in a gateway reply.
type call struct {
	name string
	args map[string]any
}

// toolCallsReply is a chat completion carrying calls as parallel tool calls. The
// arguments are built only through json.Marshal and embedded once (learning
// scripted-gateway-double-double-escapes-tool-args).
func toolCallsReply(calls ...call) string { return toolCallsReplyUsage(0, 0, calls...) }

// toolCallsReplyUsage is toolCallsReply reporting in prompt and out completion
// tokens; zero for both omits the usage object.
func toolCallsReplyUsage(in, out int, calls ...call) string {
	type fn struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	}
	type tc struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function fn     `json:"function"`
	}
	var tcs []tc
	for i, c := range calls {
		tcs = append(tcs, tc{ID: fmt.Sprintf("c%d", i), Type: "function", Function: fn{Name: c.name, Arguments: argsJSON(c.args)}})
	}
	return completionReply(map[string]any{"role": "assistant", "content": "", "tool_calls": tcs}, in, out)
}

// textReply is a plain-text chat completion.
func textReply(s string) string { return textReplyUsage(s, 0, 0) }

// textReplyUsage is textReply reporting in prompt and out completion tokens.
func textReplyUsage(s string, in, out int) string {
	return completionReply(map[string]any{"role": "assistant", "content": s}, in, out)
}

// completionReply wraps message as a one-choice chat completion, adding a
// top-level usage object only when a token count is non-zero.
func completionReply(message map[string]any, in, out int) string {
	body := map[string]any{"choices": []any{map[string]any{"message": message}}}
	if in != 0 || out != 0 {
		body["usage"] = map[string]any{"prompt_tokens": in, "completion_tokens": out}
	}
	b, _ := json.Marshal(body)
	return string(b)
}

// argsJSON is m as plain JSON, the shape Registry.Execute takes.
func argsJSON(m map[string]any) string {
	b, _ := json.Marshal(m)
	return string(b)
}

// toolResults returns the contents of c's role=="tool" messages.
func toolResults(c gatewayCall) []string {
	var out []string
	for _, m := range c.Messages {
		if m.Role == "tool" {
			out = append(out, m.Content)
		}
	}
	return out
}

// sortedToolNames returns the tool names c advertised, sorted.
func sortedToolNames(c gatewayCall) []string {
	n := c.toolNames()
	sort.Strings(n)
	return n
}

// leadRoute is the lead script: LEAD-TASK turn 0 fans out three parallel
// worker-bound spawns, later turns conclude; every READ-TASK reader concludes.
func leadRoute(task string, turn int, _ gatewayCall) string {
	switch {
	case strings.Contains(task, "LEAD-TASK") && turn == 0:
		var cs []call
		for i := 0; i < 3; i++ {
			cs = append(cs, call{name: "spawn_agent", args: map[string]any{
				"label": fmt.Sprintf("r%d", i), "task": "READ-TASK", "worker": "reader",
			}})
		}
		return toolCallsReply(cs...)
	case strings.Contains(task, "LEAD-TASK"):
		return textReply("lead done")
	case strings.Contains(task, "READ-TASK"):
		return textReply("read done")
	}
	return textReply("unscripted")
}

// wfOperatorYAML is harnessOperator with room for the lead's turns and one
// workflow whose pool admits two spawns and whose reader worker may only read.
const wfOperatorYAML = `
models:
  default: fast
  fast: {id: cloud/fast}
  house: {id: cloud/house}
max_turns: 4
throughput: {requests_per_minute: 6000}
workflows:
  wf:
    pool: {total: 2}
    workers:
      reader: {tools: [read_file]}
`

var readerSchema = []string{"blackboard_delete", "blackboard_keys", "blackboard_read", "blackboard_wait", "blackboard_write", "read_file"}

// nodeLabelled returns the single tree node labelled label.
func nodeLabelled(t *testing.T, tree *agent.AgentTree, label string) agent.NodeView {
	t.Helper()
	var found []agent.NodeView
	for _, v := range tree.SnapshotAll() {
		if v.Label == label {
			found = append(found, v)
		}
	}
	if len(found) != 1 {
		t.Fatalf("nodes labelled %q = %d, want 1", label, len(found))
	}
	return found[0]
}

func TestLocalBindingsActivateWorkflowPerSpawn(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func(t *testing.T, cfg config.Config, toolReg *tools.Registry, tree *agent.AgentTree)
	}{
		{"one-shot", func(t *testing.T, cfg config.Config, toolReg *tools.Registry, tree *agent.AgentTree) {
			reg := registryFromConfig(cfg)
			_, closeMCP := buildOneShotRuntimeDeps(cfg, reg, reg.Default, toolReg, tree, io.Discard, false, nil,
				permissions.AlwaysApprove, "block", false, nil, nil)
			t.Cleanup(closeMCP)
		}},
		{"shell", func(t *testing.T, cfg config.Config, toolReg *tools.Registry, tree *agent.AgentTree) {
			reg := registryFromConfig(cfg)
			_ = buildShellRuntimeDeps(shellDepsInput{
				cfg: cfg, reg: reg, alias: reg.Default, toolReg: toolReg, tree: tree,
				skillBlock: "block", childApprove: permissions.AlwaysApprove,
				rootApprove: permissions.AlwaysApprove,
			})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			gw := &routedGateway{route: leadRoute}
			srv := httptest.NewServer(gw)
			t.Cleanup(srv.Close)
			cfg, err := config.FromYAML([]byte(wfOperatorYAML), "test")
			if err != nil {
				t.Fatal(err)
			}
			cfg.Gateway.URL, cfg.Gateway.Key = srv.URL, "tkn"
			reg := registryFromConfig(cfg)
			toolReg := defaultToolRegistry(nil, cfg.Research, nil)
			tree := agent.NewAgentTreeWithConcurrency(reg.Default, reg.Default, cfg.Agents.MaxConcurrent)
			tc.build(t, cfg, toolReg, tree)

			res := toolReg.Execute(context.Background(), "spawn_agent",
				argsJSON(map[string]any{"label": "lead", "task": "LEAD-TASK", "workflow": "wf"}))
			if res.IsError {
				t.Fatalf("root spawn_agent: %s", res.Output)
			}

			readers := gw.callsFor("READ-TASK")
			if len(readers) != 2 {
				t.Fatalf("reader requests = %d, want 2 (pool.total 2 admits two of three)", len(readers))
			}
			for i, c := range readers {
				if got := sortedToolNames(c); !reflect.DeepEqual(got, readerSchema) {
					t.Fatalf("reader %d tools = %v, want %v", i, got, readerSchema)
				}
			}

			var lead1 *gatewayCall
			for _, c := range gw.callsFor("LEAD-TASK") {
				n := 0
				for _, m := range c.Messages {
					if m.Role == "assistant" {
						n++
					}
				}
				if n == 1 {
					c := c
					lead1 = &c
				}
			}
			if lead1 == nil {
				t.Fatal("no lead turn-1 request")
			}
			results := toolResults(*lead1)
			if len(results) != 3 {
				t.Fatalf("lead turn-1 tool results = %d, want 3: %q", len(results), results)
			}
			refused, done := 0, 0
			for _, r := range results {
				if strings.Contains(r, "workflow spawn quota exhausted") {
					refused++
				}
				if strings.Contains(r, "read done") {
					done++
				}
			}
			if refused != 1 || done != 2 {
				t.Fatalf("lead turn-1 results: %d refused, %d done, want 1 and 2: %q", refused, done, results)
			}

			lead := nodeLabelled(t, tree, "lead")
			if lead.WorkflowRoot != "wf" {
				t.Fatalf("lead WorkflowRoot = %q, want wf", lead.WorkflowRoot)
			}
			pool, ok := tree.Scheduler().Pool(lead.ID)
			if !ok || pool.Total != 2 {
				t.Fatalf("lead pool = %+v (registered %v), want Total 2", pool, ok)
			}
			nReaders := 0
			for _, v := range tree.SnapshotAll() {
				if v.ParentID != lead.ID {
					continue
				}
				nReaders++
				if got := tree.WorkflowRootOf(v.ID); got != lead.ID {
					t.Fatalf("reader %s WorkflowRootOf = %q, want lead %q", v.Label, got, lead.ID)
				}
			}
			if nReaders == 0 {
				t.Fatal("lead has no reader children")
			}
		})
	}
}

func TestResearchProbeRefusesNestedRootSpawn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	gw := &routedGateway{route: func(string, int, gatewayCall) string { return textReply("unexpected") }}
	srv := httptest.NewServer(gw)
	t.Cleanup(srv.Close)
	cfg := config.Default()
	cfg.Gateway.URL, cfg.Gateway.Key = srv.URL, "tkn"
	reg := registryFromConfig(cfg)
	toolReg := defaultToolRegistry(nil, cfg.Research, nil)
	tree := agent.NewAgentTreeWithConcurrency(reg.Default, reg.Default, cfg.Agents.MaxConcurrent)
	rootNode := tree.Node(tree.RootID())
	scope := newWorkflowScope(tree, cfg.Workflows)
	if _, err := scope.activateRoot(rootNode, "research"); err != nil {
		t.Fatalf("activateRoot: %v", err)
	}
	_ = buildResearchProbeRuntimeDeps(researchProbeDepsInput{
		cfg: cfg, reg: reg, alias: reg.Default, toolReg: toolReg, tree: tree,
		scope: scope, logSink: probe.NewLog(),
	})

	res := toolReg.Execute(context.Background(), "spawn_agent",
		argsJSON(map[string]any{"label": "x", "task": "X-TASK", "workflow": "research"}))
	if !res.IsError || !strings.Contains(res.Output, "nested workflow activation refused") {
		t.Fatalf("nested spawn = %+v, want a nested-activation refusal", res)
	}
	if n := len(gw.callsFor("X-TASK")); n != 0 {
		t.Fatalf("X-TASK requests = %d, want 0", n)
	}
}
