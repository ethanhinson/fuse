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
	"time"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/tools"
)

// plNopRenderer discards loop events; the pipeline spawn seam is what these
// tests exercise, not the display.
type plNopRenderer struct{}

func (plNopRenderer) Assistant(string)                {}
func (plNopRenderer) ToolCall(string, string)         {}
func (plNopRenderer) ToolResult(string, tools.Result) {}
func (plNopRenderer) Errorf(string, ...any)           {}
func (plNopRenderer) Tokens(int, int)                 {}

// plLastUserContent returns the content of the last user message in a gateway
// request body — for a pipeline step child that is the (substituted) step prompt,
// and for the synthesis child that is the goal.
func plLastUserContent(body []byte) string {
	var sent struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(body, &sent)
	last := ""
	for _, m := range sent.Messages {
		if m.Role == "user" {
			last = m.Content
		}
	}
	return last
}

// plSystemContent returns the concatenated system-message content of a request.
func plSystemContent(body []byte) string {
	var sent struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(body, &sent)
	var b strings.Builder
	for _, m := range sent.Messages {
		if m.Role == "system" {
			b.WriteString(m.Content)
		}
	}
	return b.String()
}

// plChildSpawner builds a real Spawner whose children are real Agents driven by
// the same scripted gateway (adapter), plus the tools.SpawnFunc-free raw spawner
// the pipeline engine consumes directly.
func plChildSpawner(tree *agent.AgentTree, rootNode *agent.AgentNode, adapter *model.Adapter) *agent.Spawner {
	childReg := tools.NewRegistry()
	return agent.NewSpawner(
		agent.WithTree(tree),
		agent.WithNode(rootNode),
		agent.WithSpawnDepth(0),
		agent.WithChildBuilder(func(ctx context.Context, opts agent.SpawnOpts, _ *agent.AgentNode, _ *agent.AgentTree) (string, error) {
			child := agent.New(adapter, childReg, plNopRenderer{}, "cloud/deepseek-v4-flash", opts.SystemPrompt, 1, 256)
			msgs, rerr := child.Run(ctx, []model.Message{{Role: "user", Content: opts.Task}})
			return childResult(msgs, rerr)
		}),
	)
}

// authoredChainYAML is a 2-step chain: step-two depends on step-one. Their
// prompts are distinctive so the gateway double can detect spawn order.
const authoredChainYAML = `
name: chain
steps:
  - name: step-one
    prompt: "PIPELINE-STEP-ONE do the first thing"
    outputs: ["one.out"]
  - name: step-two
    prompt: "PIPELINE-STEP-TWO do the second thing"
    depends_on: ["step-one"]
`

// TestPipelineGatewayAuthored drives pipeline_run({definition:<2-step chain>})
// against a real Spawner + scripted gateway. It asserts the steps spawn in
// dependency order and NO synthesis call is made.
func TestPipelineGatewayAuthored(t *testing.T) {
	tree := agent.NewAgentTree("root", "cloud/deepseek-v4-flash")
	rootNode := tree.Node(tree.RootID())
	bb := agent.NewBlackboard(tree)

	var (
		mu       sync.Mutex
		order    []string
		sawSynth bool
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")

		sys := plSystemContent(body)
		user := plLastUserContent(body)

		mu.Lock()
		if strings.Contains(sys, "pipeline synthesizer") {
			sawSynth = true
		}
		switch {
		case strings.Contains(user, "PIPELINE-STEP-ONE"):
			order = append(order, "step-one")
		case strings.Contains(user, "PIPELINE-STEP-TWO"):
			order = append(order, "step-two")
		}
		mu.Unlock()

		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"done"}}]}`)
	}))
	defer srv.Close()

	adapter := model.NewAdapter(srv.URL, "tkn", srv.Client())
	spawner := plChildSpawner(tree, rootNode, adapter)

	tool := tools.NewPipelineRunTool(
		makePipelineRunFn(spawner, bb, tree.Scheduler(), rootNode),
		makePipelineSynthFn(spawner, bb, tree.Scheduler(), rootNode, config.Default(), nil, nil, nil),
	)

	args, _ := json.Marshal(map[string]any{"definition": authoredChainYAML})
	res := tool.Execute(context.Background(), string(args))
	if res.IsError {
		t.Fatalf("authored pipeline_run errored: %s", res.Output)
	}
	if !strings.Contains(res.Output, "completed") {
		t.Errorf("status = %q, want completed", res.Output)
	}

	mu.Lock()
	defer mu.Unlock()
	if sawSynth {
		t.Error("authored path must NOT make a synthesis call")
	}
	if len(order) != 2 {
		t.Fatalf("expected 2 step spawns, got %v", order)
	}
	if order[0] != "step-one" || order[1] != "step-two" {
		t.Errorf("steps spawned out of dependency order: %v", order)
	}
	// pipeline.status is written by the engine.
	if e, ok := bb.Get("pipeline.status"); !ok || e.Value != "completed" {
		t.Errorf("pipeline.status = %v (ok=%v), want completed", e.Value, ok)
	}
}

// TestPipelineRunYieldsSlotUnderTightCap regresses FIX 1 (change 0026): when
// pipeline_run is invoked from a NON-ROOT child (depth≥1, which holds a scheduler
// slot) under a TIGHT slot cap (max_concurrent=1), the run fn must yield the
// calling node's slot around pipeline.Run — otherwise the depth-2 step children it
// spawns queue forever behind the slot their own ancestor is holding (deadlock;
// learning: slot-cap-yield-while-blocked-on-children). It asserts the pipeline
// completes within a timeout.
//
// Shape: root spawns ONE depth-1 child (consuming the only slot); that child's
// builder invokes the pipeline_run fn with a depth-1 spawner. The pipeline is a
// 2-step chain, so each step child (depth 2) needs the one slot the depth-1
// parent holds. Without the yield the second Spawn parks forever.
func TestPipelineRunYieldsSlotUnderTightCap(t *testing.T) {
	// Cap the whole tree at ONE running child.
	tree := agent.NewAgentTreeWithConcurrency("root", "cloud/deepseek-v4-flash", 1)
	rootNode := tree.Node(tree.RootID())
	bb := agent.NewBlackboard(tree)
	sched := tree.Scheduler()

	// A scripted child builder that just returns "done" (no gateway needed here —
	// the deadlock is about slot admission, not model wire behavior).
	build := func(ctx context.Context, opts agent.SpawnOpts, _ *agent.AgentNode, _ *agent.AgentTree) (string, error) {
		return "done", nil
	}

	// Depth-1 spawner factory: the pipeline (invoked inside the depth-1 child)
	// spawns its step children from here, at depth 2.
	depth1Spawner := func(node *agent.AgentNode) *agent.Spawner {
		return agent.NewSpawner(
			agent.WithTree(tree),
			agent.WithNode(node),
			agent.WithSpawnDepth(1),
			agent.WithChildBuilder(build),
		)
	}

	// The root spawner's depth-1 child builder runs pipeline_run as that child,
	// holding the single slot while the pipeline needs it for its step children.
	rootSpawner := agent.NewSpawner(
		agent.WithTree(tree),
		agent.WithNode(rootNode),
		agent.WithSpawnDepth(0),
		agent.WithChildBuilder(func(ctx context.Context, opts agent.SpawnOpts, childNode *agent.AgentNode, _ *agent.AgentTree) (string, error) {
			runFn := makePipelineRunFn(depth1Spawner(childNode), bb, sched, childNode)
			return runFn(ctx, []byte(authoredChainYAML))
		}),
	)

	done := make(chan struct{})
	var spawnErr error
	go func() {
		defer close(done)
		h, err := rootSpawner.Spawn(context.Background(), agent.SpawnOpts{Label: "runner", Task: "run the pipeline"})
		if err != nil {
			spawnErr = err
			return
		}
		d := h.Wait()
		spawnErr = d.Err
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("pipeline_run deadlocked: depth-1 caller did not yield its slot under a tight cap")
	}
	if spawnErr != nil {
		t.Fatalf("pipeline_run under tight cap errored: %v", spawnErr)
	}
	if e, ok := bb.Get("pipeline.status"); !ok || e.Value != "completed" {
		t.Fatalf("pipeline.status = %v (ok=%v), want completed", e.Value, ok)
	}
}

// synthChainJSON is the Pipeline the synthesis turn returns: a workflow-bound
// (RequirePool) 2-step chain with distinctive step prompts.
const synthChainJSON = `{"name":"synth-chain","workflow":"research","steps":[` +
	`{"name":"s1","prompt":"SYNTH-STEP-ONE first"},` +
	`{"name":"s2","prompt":"SYNTH-STEP-TWO second","depends_on":["s1"]}]}`

// TestPipelineGatewaySynthesized drives pipeline_run({goal:...}). The gateway's
// synthesis turn returns a structured Pipeline; subsequent turns drive the step
// spawns. It asserts synthesis happens first, then the steps spawn in order, and
// pipeline.plan is populated.
func TestPipelineGatewaySynthesized(t *testing.T) {
	tree := agent.NewAgentTree("root", "cloud/deepseek-v4-flash")
	rootNode := tree.Node(tree.RootID())
	bb := agent.NewBlackboard(tree)

	var (
		mu  sync.Mutex
		seq []string // ordered record of "synth", "s1", "s2"
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")

		sys := plSystemContent(body)
		user := plLastUserContent(body)

		mu.Lock()
		switch {
		case strings.Contains(sys, "pipeline synthesizer"):
			seq = append(seq, "synth")
			mu.Unlock()
			// Return the pipeline JSON as the synthesis child's final message.
			payload := map[string]any{"choices": []any{map[string]any{
				"message": map[string]any{"role": "assistant", "content": synthChainJSON},
			}}}
			b, _ := json.Marshal(payload)
			w.Write(b)
			return
		case strings.Contains(user, "SYNTH-STEP-ONE"):
			seq = append(seq, "s1")
		case strings.Contains(user, "SYNTH-STEP-TWO"):
			seq = append(seq, "s2")
		}
		mu.Unlock()
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"done"}}]}`)
	}))
	defer srv.Close()

	adapter := model.NewAdapter(srv.URL, "tkn", srv.Client())
	spawner := plChildSpawner(tree, rootNode, adapter)

	// Config with a small attempt budget; RequirePool is set by makePipelineSynthFn.
	cfg := config.Default()
	tool := tools.NewPipelineRunTool(
		makePipelineRunFn(spawner, bb, tree.Scheduler(), rootNode),
		makePipelineSynthFn(spawner, bb, tree.Scheduler(), rootNode, cfg, []string{"facet-researcher"}, []string{"read_file"}, nil),
	)

	args, _ := json.Marshal(map[string]any{"goal": "produce a synthesized report"})
	res := tool.Execute(context.Background(), string(args))
	if res.IsError {
		t.Fatalf("synthesized pipeline_run errored: %s", res.Output)
	}
	if !strings.Contains(res.Output, "completed") {
		t.Errorf("status = %q, want completed", res.Output)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seq) < 3 {
		t.Fatalf("expected synth + 2 step spawns, got %v", seq)
	}
	if seq[0] != "synth" {
		t.Errorf("synthesis call must happen first, sequence = %v", seq)
	}
	// The two step spawns follow synthesis, in dependency order.
	if seq[1] != "s1" || seq[2] != "s2" {
		t.Errorf("steps spawned out of order after synthesis: %v", seq)
	}
	// pipeline.plan must be populated with the generated DAG.
	planEntry, ok := bb.Get("pipeline.plan")
	if !ok {
		t.Fatal("pipeline.plan not populated on the blackboard")
	}
	planJSON, _ := json.Marshal(planEntry.Value)
	if !strings.Contains(string(planJSON), "synth-chain") {
		t.Errorf("pipeline.plan does not carry the synthesized DAG: %s", planJSON)
	}
}
