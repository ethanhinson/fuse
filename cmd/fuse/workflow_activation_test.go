package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/ethanhinson/fuse/internal/agent"
)

// Helpers shared by the binding acceptance tests: the real deps builders, driven

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
