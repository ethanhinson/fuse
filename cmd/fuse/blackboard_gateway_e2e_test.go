package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/tools"
)

// gwNopRenderer discards all loop events; the gateway seam is what this test
// exercises, not the display.
type gwNopRenderer struct{}

func (gwNopRenderer) Assistant(string)                {}
func (gwNopRenderer) ToolCall(string, string)         {}
func (gwNopRenderer) ToolResult(string, tools.Result) {}
func (gwNopRenderer) Errorf(string, ...any)           {}
func (gwNopRenderer) Tokens(int, int)                 {}

// TestGatewaySeamOffersBlackboardTools stands up a real agent turn through the
// production wiring — the real model.Adapter POSTing to a scripted httptest
// gateway — and asserts that all five blackboard tools appear in the request's
// tools[] array the model actually sees. The teatest harness fakes the Completer
// seam and never proves this; this test drives the real adapter end-to-end.
func TestGatewaySeamOffersBlackboardTools(t *testing.T) {
	var (
		mu       sync.Mutex
		gotTools map[string]bool
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		var sent struct {
			Tools []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		if err := json.Unmarshal(b, &sent); err != nil {
			t.Errorf("unmarshal request body: %v", err)
		}
		mu.Lock()
		gotTools = make(map[string]bool, len(sent.Tools))
		for _, tl := range sent.Tools {
			gotTools[tl.Function.Name] = true
		}
		mu.Unlock()

		// End the turn without tool calls so the agent loop terminates in one turn.
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"done"}}]}`)
	}))
	defer srv.Close()

	// Build a root tool registry exactly the production way: default tools plus
	// the root-bound blackboard tools.
	tree := agent.NewAgentTree("root", "cloud/deepseek-v4-flash")
	rootNode := tree.Node(tree.RootID())
	bb := agent.NewBlackboard(tree)

	reg := tools.NewRegistry()
	for _, tl := range tools.DefaultTools(nil) {
		reg.Register(tl)
	}
	for _, tl := range tools.NewBlackboardTools(bb.ForNode(rootNode)) {
		reg.Register(tl)
	}

	// A real Agent wired to the real model.Adapter pointed at the scripted
	// gateway. The registry satisfies agent.ToolExecutor directly (Schemas +
	// Execute), so its schemas flow through the adapter into the request body.
	a := agent.New(
		model.NewAdapter(srv.URL, "tkn", srv.Client()),
		reg,
		gwNopRenderer{},
		"cloud/deepseek-v4-flash",
		"",
		1,   // maxTurns
		128, // maxTokens
	)

	if _, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatalf("agent run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if gotTools == nil {
		t.Fatal("gateway never received a request")
	}
	for _, n := range blackboardToolNames {
		if !gotTools[n] {
			t.Errorf("blackboard tool %q did not reach the gateway; tools seen: %v", n, sortedKeys(gotTools))
		}
	}
}
