package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/tools"
)

// sdNopRenderer discards loop events; the spawn/validation seam is what this
// test exercises, not the display.
type sdNopRenderer struct{}

func (sdNopRenderer) Assistant(string)                {}
func (sdNopRenderer) ToolCall(string, string)         {}
func (sdNopRenderer) ToolResult(string, tools.Result) {}
func (sdNopRenderer) Errorf(string, ...any)           {}
func (sdNopRenderer) Tokens(int, int)                 {}

// expectsSchema is the JSON Schema the parent turn attaches to its spawn_agent
// call in these tests.
const expectsSchema = `{"type":"object","properties":{"answer":{"type":"integer"}},"required":["answer"]}`

// sdReqHasSpawnAgent reports whether the request the gateway received offers the
// spawn_agent tool — the discriminator between the parent turn (offers it) and
// the child turn (does not).
func sdReqHasSpawnAgent(body []byte) bool {
	var sent struct {
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	_ = json.Unmarshal(body, &sent)
	for _, tl := range sent.Tools {
		if tl.Function.Name == "spawn_agent" {
			return true
		}
	}
	return false
}

// sdReqHasToolResult reports whether the request messages already carry a tool
// result — i.e. this is the parent's SECOND turn (post-spawn), which should end.
func sdReqHasToolResult(body []byte) bool {
	var sent struct {
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(body, &sent)
	for _, m := range sent.Messages {
		if m.Role == "tool" {
			return true
		}
	}
	return false
}

// runStructuredDelegationSeam drives a real root Agent (real model.Adapter →
// scripted gateway) that spawns one real child Agent through a real Spawner. The
// expected-result schema is attached on the CODE path — the spawnFn (standing in
// for the pipeline/cmd wiring) sets agent.SpawnOpts.Expects directly — because as
// of change 0042 (D7) the freeform spawn_agent TOOL is prose-only and never
// carries `expects`. The parent's tool call still emits an `expects` key in its
// args to prove the tool drops it (it must NOT reach the SpawnRequest); the
// schema that actually drives validation comes from the code path. childJSON is
// what the child turn returns as its final message. It returns the parent's
// spawn_agent tool result text (which carries the schema note). This is the
// real-binary seam the teatest harness never reaches (learning
// verify-tool-loop-at-gateway-seam).
func runStructuredDelegationSeam(t *testing.T, childJSON string) string {
	t.Helper()

	tree := agent.NewAgentTree("root", "cloud/deepseek-v4-flash")
	rootNode := tree.Node(tree.RootID())

	var (
		mu             sync.Mutex
		toolResultText string
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")

		switch {
		case !sdReqHasSpawnAgent(body):
			// Child turn: return the scripted final JSON as the assistant message.
			payload := map[string]any{
				"choices": []any{map[string]any{
					"message": map[string]any{"role": "assistant", "content": childJSON},
				}},
			}
			b, _ := json.Marshal(payload)
			w.Write(b)
		case sdReqHasToolResult(body):
			// Parent's second turn: the spawn tool result is in the messages;
			// capture it and end the turn plainly.
			mu.Lock()
			var sent struct {
				Messages []struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				} `json:"messages"`
			}
			_ = json.Unmarshal(body, &sent)
			for _, m := range sent.Messages {
				if m.Role == "tool" {
					toolResultText = m.Content
				}
			}
			mu.Unlock()
			io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"done"}}]}`)
		default:
			// Parent's first turn: call spawn_agent. It includes an `expects` key in
			// its args on purpose — the prose-only tool (0042 D7) must DROP it, which
			// spawnFn asserts. The schema that drives validation is attached on the
			// code path instead.
			args := `{"label":"kid","task":"compute","expects":` + expectsSchema + `}`
			b, _ := json.Marshal(args)
			io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"",`+
				`"tool_calls":[{"id":"call_1","type":"function","function":{"name":"spawn_agent","arguments":`+
				string(b)+`}}]}}]}`)
		}
	}))
	defer srv.Close()

	adapter := model.NewAdapter(srv.URL, "tkn", srv.Client())

	// Child registry: NO spawn_agent, so child requests are distinguishable and
	// the child cannot recurse.
	childReg := tools.NewRegistry()

	// Real Spawner wiring a real child Agent against the same gateway.
	spawner := agent.NewSpawner(
		agent.WithTree(tree),
		agent.WithNode(rootNode),
		agent.WithSpawnDepth(0),
		agent.WithChildBuilder(func(ctx context.Context, opts agent.SpawnOpts, _ *agent.AgentNode, _ *agent.AgentTree) (string, error) {
			child := agent.New(adapter, childReg, sdNopRenderer{}, "cloud/deepseek-v4-flash", opts.SystemPrompt, 1, 256)
			msgs, rerr := child.Run(ctx, []model.Message{{Role: "user", Content: opts.Task}})
			return childResult(msgs, rerr)
		}),
	)

	spawnFn := func(ctx context.Context, req tools.SpawnRequest) (string, error) {
		// The freeform tool is prose-only (0042 D7): an `expects` key in the model's
		// args must be dropped, so req.Expects is nil here. Guard that, then attach
		// the schema on the code path — the surviving structured-delegation seam
		// (agent.SpawnOpts.Expects), which the pipeline/cmd wiring drives directly.
		if req.Expects != nil {
			t.Errorf("tool must drop `expects` from args; req.Expects = %#v", req.Expects)
		}
		var expects any
		_ = json.Unmarshal([]byte(expectsSchema), &expects)
		h, herr := spawner.Spawn(ctx, agent.SpawnOpts{
			Label: req.Label, Task: req.Task, SystemPrompt: req.SystemPrompt,
			ModelID: req.Model, Tools: req.Tools, Worker: req.Worker, Expects: expects,
		})
		if herr != nil {
			return "", herr
		}
		d := h.Wait()
		return d.Result, d.Err
	}

	// Root registry offers the real spawn_agent tool.
	rootReg := tools.NewRegistry()
	rootReg.Register(tools.NewSpawnAgentTool(spawnFn))

	root := agent.New(adapter, rootReg, sdNopRenderer{}, "cloud/deepseek-v4-flash", "", 3, 256)
	if _, err := root.Run(context.Background(), []model.Message{{Role: "user", Content: "spawn a child"}}); err != nil {
		t.Fatalf("root run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	return toolResultText
}

// TestStructuredDelegationSeam_Match: a child returning conforming JSON yields a
// parent spawn_agent tool result carrying the match note, with no spawn error.
func TestStructuredDelegationSeam_Match(t *testing.T) {
	got := runStructuredDelegationSeam(t, `{"answer":42}`)
	if got == "" {
		t.Fatal("parent never received a spawn_agent tool result")
	}
	if !strings.Contains(got, "(matched expected schema)") {
		t.Errorf("expected match note in tool result, got:\n%s", got)
	}
	if strings.Contains(got, "did NOT match") {
		t.Errorf("conforming JSON must not carry a mismatch note, got:\n%s", got)
	}
}

// TestStructuredDelegationSeam_Mismatch: a child returning non-conforming JSON
// yields the raw text plus the mismatch note, and the spawn does not error (the
// parent turn completed and captured the tool result).
func TestStructuredDelegationSeam_Mismatch(t *testing.T) {
	got := runStructuredDelegationSeam(t, `{"answer":"not-an-int"}`)
	if got == "" {
		t.Fatal("parent never received a spawn_agent tool result")
	}
	if !strings.Contains(got, "result did NOT match expected schema:") {
		t.Errorf("expected mismatch note in tool result, got:\n%s", got)
	}
	if !strings.Contains(got, `"answer":"not-an-int"`) {
		t.Errorf("mismatch must preserve the child's raw text, got:\n%s", got)
	}
}
