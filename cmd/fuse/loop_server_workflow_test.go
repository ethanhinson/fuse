package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"

	harnessv1 "github.com/ethanhinson/fuse/harness/v1"
	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/runtime"
	"github.com/ethanhinson/fuse/internal/tools"
)

// Acceptance for per-spawn workflow activation (change 0091) on the hosted
// binding: the production loop-server factory, a spec-defined workflow
// (ADR-0061) and a scripted gateway.

// treeCapture wraps the production harness resolver and records the tree each
// loop's BuildAgent is handed, so a test can inspect the loop's nodes.
type treeCapture struct {
	inner runtime.HarnessResolver
	mu    sync.Mutex
	trees []*agent.AgentTree
}

func (c *treeCapture) ResolveHarness(ctx context.Context, tenant event.TenantID, spec *harnessv1.HarnessSpec) (runtime.LoopFactory, error) {
	f, err := c.inner.ResolveHarness(ctx, tenant, spec)
	if err != nil || f.BuildAgent == nil {
		return f, err
	}
	build := f.BuildAgent
	f.BuildAgent = func(store event.EventStore, tree *agent.AgentTree, modelID string, toolReg *tools.Registry) (*agent.Agent, agent.ChildBuilder, string, error) {
		c.mu.Lock()
		c.trees = append(c.trees, tree)
		c.mu.Unlock()
		return build(store, tree, modelID, toolReg)
	}
	return f, nil
}

func (c *treeCapture) last() *agent.AgentTree {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.trees) == 0 {
		return nil
	}
	return c.trees[len(c.trees)-1]
}

// rawCapture records every raw request body before handing it to next, for
// assertions on fields gatewayCall does not decode (tool schemas).
type rawCapture struct {
	next   http.Handler
	mu     sync.Mutex
	bodies [][]byte
}

func (r *rawCapture) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	r.bodies = append(r.bodies, body)
	r.mu.Unlock()
	req.Body = io.NopCloser(bytes.NewReader(body))
	r.next.ServeHTTP(w, req)
}

// firstFor returns the first raw body whose first user message contains marker.
func (r *rawCapture) firstFor(t *testing.T, marker string) []byte {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, b := range r.bodies {
		var c gatewayCall
		_ = json.Unmarshal(b, &c)
		for _, m := range c.Messages {
			if m.Role == "user" {
				if strings.Contains(m.Content, marker) {
					return b
				}
				break
			}
		}
	}
	t.Fatalf("no request for %q", marker)
	return nil
}

// wfSpec is the spec-defined workflow every hosted test runs: a pool of two
// spawns and a reader worker that may only read.
func wfSpec() *harnessv1.HarnessSpec {
	return &harnessv1.HarnessSpec{Workflows: map[string]*harnessv1.Workflow{
		"wf": {Pool: &harnessv1.Pool{Total: 2}, Workers: map[string]*harnessv1.Worker{"reader": {Tools: []string{"read_file"}}}},
	}}
}

// hostedOperatorYAML is harnessOperator with room for the lead's turns.
const hostedOperatorYAML = `
models:
  default: fast
  fast: {id: cloud/fast}
  house: {id: cloud/house}
max_turns: 4
throughput: {requests_per_minute: 6000}
`

// runHosted starts one wfSpec loop on the production loop-server factory over
// a routed gateway and waits for it, returning the gateway, the raw bodies and
// the loop's tree.
func runHosted(t *testing.T, route func(task string, turn int, c gatewayCall) string) (*routedGateway, *rawCapture, *agent.AgentTree) {
	t.Helper()
	r := runHostedSpec(t, wfSpec(), route)
	return r.gw, r.raw, r.tree
}

// hostedRun is one finished hosted loop: the gateway, the raw bodies, the
// loop's tree, and the runtime and loop id for reading its durable history.
type hostedRun struct {
	gw     *routedGateway
	raw    *rawCapture
	tree   *agent.AgentTree
	rt     runtime.Runtime
	loopID string
}

// runHostedSpec starts one loop for spec on the production loop-server factory
// over a routed gateway and waits for it.
func runHostedSpec(t *testing.T, spec *harnessv1.HarnessSpec, route func(task string, turn int, c gatewayCall) string) hostedRun {
	t.Helper()
	gw := &routedGateway{route: route}
	raw := &rawCapture{next: gw}
	deps, host := harnessServerWith(t, hostedOperatorYAML, raw)
	capture := &treeCapture{inner: host}
	deps.Harness = capture
	rt := runtime.New(deps)
	h, err := rt.StartLoop(context.Background(), runtime.LoopConfig{Task: "ROOT-TASK", Harness: spec})
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	_, _ = h.Wait()
	tree := capture.last()
	if tree == nil {
		t.Fatal("the loop's BuildAgent never ran")
	}
	return hostedRun{gw: gw, raw: raw, tree: tree, rt: rt, loopID: h.ID()}
}

// turnRequest returns the request for marker that carries exactly turn
// assistant messages.
func turnRequest(t *testing.T, gw *routedGateway, marker string, turn int) gatewayCall {
	t.Helper()
	for _, c := range gw.callsFor(marker) {
		n := 0
		for _, m := range c.Messages {
			if m.Role == "assistant" {
				n++
			}
		}
		if n == turn {
			return c
		}
	}
	t.Fatalf("no %s turn-%d request", marker, turn)
	return gatewayCall{}
}

// countContaining reports how many of results contain sub.
func countContaining(results []string, sub string) int {
	n := 0
	for _, r := range results {
		if strings.Contains(r, sub) {
			n++
		}
	}
	return n
}

func TestHostedWorkflowSpawnEnforcesPoolAndWorkerTools(t *testing.T) {
	gw, raw, tree := runHosted(t, func(task string, turn int, c gatewayCall) string {
		switch {
		case strings.Contains(task, "ROOT-TASK") && turn == 0:
			return toolCallsReply(call{name: "spawn_agent", args: map[string]any{
				"label": "lead", "task": "LEAD-TASK", "workflow": "wf",
			}})
		case strings.Contains(task, "ROOT-TASK"):
			return textReply("root done")
		}
		return leadRoute(task, turn, c)
	})

	// 1. The root is offered spawn_agent with wf advertised in its schema.
	var root0 struct {
		Tools []struct {
			Function struct {
				Name       string          `json:"name"`
				Parameters json.RawMessage `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw.firstFor(t, "ROOT-TASK"), &root0); err != nil {
		t.Fatal(err)
	}
	var spawnSchema json.RawMessage
	for _, tl := range root0.Tools {
		if tl.Function.Name == "spawn_agent" {
			spawnSchema = tl.Function.Parameters
		}
	}
	if spawnSchema == nil {
		t.Fatal("root turn 0 does not offer spawn_agent")
	}
	var params struct {
		Properties map[string]struct {
			Enum []string `json:"enum"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(spawnSchema, &params); err != nil {
		t.Fatal(err)
	}
	wfProp, ok := params.Properties["workflow"]
	listed := false
	for _, name := range wfProp.Enum {
		listed = listed || name == "wf"
	}
	if !ok || !listed {
		t.Fatalf("root spawn_agent workflow property = %+v (present %v), want an enum listing wf: %s", wfProp, ok, spawnSchema)
	}

	// 2. The pool admits two of three readers, each with the worker's tools.
	readers := gw.callsFor("READ-TASK")
	if len(readers) != 2 {
		t.Fatalf("reader requests = %d, want 2 (pool.total 2 admits two of three)", len(readers))
	}
	for i, c := range readers {
		if got := sortedToolNames(c); !reflect.DeepEqual(got, readerSchema) {
			t.Fatalf("reader %d tools = %v, want %v", i, got, readerSchema)
		}
	}

	// 3. The lead sees one quota refusal and two reader results.
	results := toolResults(turnRequest(t, gw, "LEAD-TASK", 1))
	if len(results) != 3 {
		t.Fatalf("lead turn-1 tool results = %d, want 3: %q", len(results), results)
	}
	if r, d := countContaining(results, "workflow spawn quota exhausted"), countContaining(results, "read done"); r != 1 || d != 2 {
		t.Fatalf("lead turn-1 results: %d refused, %d done, want 1 and 2: %q", r, d, results)
	}

	// 4. The lead is the workflow root; its pool is registered on the loop's tree.
	lead := nodeLabelled(t, tree, "lead")
	if lead.WorkflowRoot != "wf" || lead.ParentID != tree.RootID() {
		t.Fatalf("lead = WorkflowRoot %q parent %q, want wf under root %q", lead.WorkflowRoot, lead.ParentID, tree.RootID())
	}
	if v := tree.Node(tree.RootID()).Snapshot(); v.WorkflowRoot != "" {
		t.Fatalf("root WorkflowRoot = %q, want empty", v.WorkflowRoot)
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
		if v.WorkflowRoot != "" {
			t.Fatalf("reader %s WorkflowRoot = %q, want empty", v.Label, v.WorkflowRoot)
		}
		if got := tree.WorkflowRootOf(v.ID); got != lead.ID {
			t.Fatalf("reader %s WorkflowRootOf = %q, want lead %q", v.Label, got, lead.ID)
		}
	}
	if nReaders == 0 {
		t.Fatal("lead has no reader children")
	}
}

func TestHostedWorkflowRefusals(t *testing.T) {
	gw, _, tree := runHosted(t, func(task string, turn int, _ gatewayCall) string {
		switch {
		case strings.Contains(task, "ROOT-TASK") && turn == 0:
			return toolCallsReply(
				call{name: "spawn_agent", args: map[string]any{"label": "bad", "task": "BAD-TASK", "workflow": "nope"}},
				call{name: "spawn_agent", args: map[string]any{"label": "lead", "task": "LEAD-TASK", "workflow": "wf"}},
				call{name: "spawn_agent", args: map[string]any{"label": "w", "task": "W-TASK", "worker": "reader"}},
			)
		case strings.Contains(task, "LEAD-TASK") && turn == 0:
			return toolCallsReply(
				call{name: "spawn_agent", args: map[string]any{"label": "u", "task": "U-TASK", "worker": "unknown"}},
				call{name: "spawn_agent", args: map[string]any{"label": "n", "task": "N-TASK", "workflow": "wf"}},
			)
		}
		return textReply("done")
	})

	root1 := toolResults(turnRequest(t, gw, "ROOT-TASK", 1))
	if countContaining(root1, `unknown workflow "nope"`) != 1 {
		t.Fatalf("root turn-1 results lack the unknown-workflow refusal: %q", root1)
	}
	if countContaining(root1, `worker "reader" named outside any workflow; pass workflow`) != 1 {
		t.Fatalf("root turn-1 results lack exactly one outside-workflow worker refusal: %q", root1)
	}
	lead1 := toolResults(turnRequest(t, gw, "LEAD-TASK", 1))
	if countContaining(lead1, `unknown worker "unknown"`) != 1 || countContaining(lead1, "nested workflow activation refused") != 1 {
		t.Fatalf("lead turn-1 results lack the worker and nesting refusals: %q", lead1)
	}
	for _, marker := range []string{"BAD-TASK", "U-TASK", "N-TASK", "W-TASK"} {
		if n := len(gw.callsFor(marker)); n != 0 {
			t.Fatalf("%s requests = %d, want 0", marker, n)
		}
	}
	for _, v := range tree.SnapshotAll() {
		switch v.Label {
		case "bad", "u", "n", "w":
			t.Fatalf("a refused spawn created node %q", v.Label)
		}
	}
}

// pool.tokens on the hosted binding: once the workflow subtree's spend reaches
// the pool, the call gate refuses the next model call of every node in that
// subtree (the reader and the lead that roots it) before it reaches the
// gateway, while the root outside the workflow keeps running.
func TestHostedWorkflowTokenPoolRefusesWorkerModelCall(t *testing.T) {
	spec := &harnessv1.HarnessSpec{Workflows: map[string]*harnessv1.Workflow{
		"wf": {Pool: &harnessv1.Pool{Total: 3, Tokens: 100}, Workers: map[string]*harnessv1.Worker{"reader": {Tools: []string{"read_file"}}}},
	}}
	r := runHostedSpec(t, spec, func(task string, turn int, _ gatewayCall) string {
		switch {
		case strings.Contains(task, "ROOT-TASK") && turn == 0:
			return toolCallsReply(call{name: "spawn_agent", args: map[string]any{"label": "lead", "task": "LEAD-TASK", "workflow": "wf"}})
		case strings.Contains(task, "ROOT-TASK"):
			return textReply("root done")
		case strings.Contains(task, "LEAD-TASK") && turn == 0:
			return toolCallsReply(call{name: "spawn_agent", args: map[string]any{"label": "r0", "task": "READ-TASK", "worker": "reader"}})
		case strings.Contains(task, "LEAD-TASK"):
			return textReply("lead done")
		case strings.Contains(task, "READ-TASK") && turn == 0:
			return toolCallsReplyUsage(80, 40, call{name: "blackboard_write", args: map[string]any{"key": "k", "value": "v"}})
		}
		return textReply("read done")
	})
	gw, tree := r.gw, r.tree

	// 1. The reader's second model call never reaches the gateway.
	if n := len(gw.callsFor("READ-TASK")); n != 1 {
		t.Fatalf("reader requests = %d, want 1 (spend 120 >= pool.tokens 100 refuses the second call)", n)
	}
	// 2. The lead roots the workflow; its call gate counts the reader's spend.
	if n := len(gw.callsFor("LEAD-TASK")); n != 1 {
		t.Fatalf("lead requests = %d, want 1 (the subtree spend refuses its turn-1 call)", n)
	}
	// 3. The root sits outside the subtree quota and sees the lead fail.
	root1 := toolResults(turnRequest(t, gw, "ROOT-TASK", 1))
	if len(root1) != 1 || !strings.HasPrefix(root1[0], "spawn_agent:") || !strings.Contains(root1[0], "workflow 120/100 tokens used") {
		t.Fatalf("root turn-1 results = %q, want one spawn_agent error carrying workflow 120/100 tokens used", root1)
	}

	lead := nodeLabelled(t, tree, "lead")
	reader := nodeLabelled(t, tree, "r0")

	// 4. The durable history records the refusal on both nodes.
	events, err := r.rt.Attach(context.Background(), "", r.loopID, 0)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	refused := map[string]bool{}
	for _, e := range events {
		if e.Kind == event.KindError && strings.Contains(string(e.Payload), "tokens used") {
			refused[e.NodeID] = true
		}
	}
	if !refused[reader.ID] || !refused[lead.ID] {
		t.Fatalf("durable token refusals: reader %v lead %v, want both (events by node: %v)", refused[reader.ID], refused[lead.ID], refused)
	}

	// 5. The tree accounts the spend against the lead's registered pool.
	if got := tree.SubtreeTokens(lead.ID); got != 120 {
		t.Fatalf("SubtreeTokens(lead) = %d, want 120", got)
	}
	if pool, ok := tree.Scheduler().Pool(lead.ID); !ok || pool.Tokens != 100 {
		t.Fatalf("lead pool = %+v (registered %v), want Tokens 100", pool, ok)
	}
	if reader.Status != agent.StatusError {
		t.Fatalf("reader status = %v, want %v", reader.Status, agent.StatusError)
	}
}

func TestHostedPipelineRunWithWorkflowBindsStageWorkers(t *testing.T) {
	gw, _, tree := runHosted(t, func(task string, turn int, _ gatewayCall) string {
		switch {
		case strings.Contains(task, "ROOT-TASK") && turn == 0:
			return toolCallsReply(call{name: "pipeline_run", args: map[string]any{
				"workflow":   "wf",
				"definition": "name: p\nsteps:\n  - name: s1\n    worker: reader\n    prompt: STEP-TASK\n",
			}})
		case strings.Contains(task, "STEP-TASK"):
			return textReply("step done")
		}
		return textReply("root done")
	})

	steps := gw.callsFor("STEP-TASK")
	if len(steps) != 1 {
		t.Fatalf("step requests = %d, want 1", len(steps))
	}
	if got := sortedToolNames(steps[0]); !reflect.DeepEqual(got, readerSchema) {
		t.Fatalf("step tools = %v, want %v", got, readerSchema)
	}
	if r := toolResults(turnRequest(t, gw, "ROOT-TASK", 1)); countContaining(r, "pipeline p: completed") != 1 {
		t.Fatalf("root turn-1 results lack the pipeline completion: %q", r)
	}

	coord := nodeLabelled(t, tree, "pipeline p")
	if coord.WorkflowRoot != "wf" || coord.ParentID != tree.RootID() {
		t.Fatalf("coordinator = WorkflowRoot %q parent %q, want wf under root %q", coord.WorkflowRoot, coord.ParentID, tree.RootID())
	}
	pool, ok := tree.Scheduler().Pool(coord.ID)
	if !ok || pool.Total != 2 {
		t.Fatalf("coordinator pool = %+v (registered %v), want Total 2", pool, ok)
	}
	nSteps := 0
	for _, v := range tree.SnapshotAll() {
		if v.ParentID != coord.ID {
			continue
		}
		nSteps++
		if got := tree.WorkflowRootOf(v.ID); got != coord.ID {
			t.Fatalf("step %s WorkflowRootOf = %q, want coordinator %q", v.Label, got, coord.ID)
		}
	}
	if nSteps != 1 {
		t.Fatalf("coordinator children = %d, want 1", nSteps)
	}
}

// A definition that names its workflow activates it when the pipeline_run tool
// argument is absent: step worker: names bind to that workflow's workers.
func TestHostedPipelineRunDefinitionWorkflowActivates(t *testing.T) {
	gw, _, tree := runHosted(t, func(task string, turn int, _ gatewayCall) string {
		switch {
		case strings.Contains(task, "ROOT-TASK") && turn == 0:
			return toolCallsReply(call{name: "pipeline_run", args: map[string]any{
				"definition": "name: p\nworkflow: wf\nsteps:\n  - name: s1\n    worker: reader\n    prompt: STEP-TASK\n",
			}})
		case strings.Contains(task, "STEP-TASK"):
			return textReply("step done")
		}
		return textReply("root done")
	})

	steps := gw.callsFor("STEP-TASK")
	if len(steps) != 1 {
		t.Fatalf("step requests = %d, want 1", len(steps))
	}
	if got := sortedToolNames(steps[0]); !reflect.DeepEqual(got, readerSchema) {
		t.Fatalf("step tools = %v, want exactly the reader allowlist plus blackboard tools %v", got, readerSchema)
	}
	if coord := nodeLabelled(t, tree, "pipeline p"); coord.WorkflowRoot != "wf" {
		t.Fatalf("coordinator WorkflowRoot = %q, want wf", coord.WorkflowRoot)
	}
}

// An argument and a definition that name different workflows is a tool error;
// nothing is spawned.
func TestHostedPipelineRunWorkflowConflictIsToolError(t *testing.T) {
	gw, _, tree := runHosted(t, func(task string, turn int, _ gatewayCall) string {
		switch {
		case strings.Contains(task, "ROOT-TASK") && turn == 0:
			return toolCallsReply(call{name: "pipeline_run", args: map[string]any{
				"workflow":   "wf",
				"definition": "name: p\nworkflow: other\nsteps:\n  - name: s1\n    worker: reader\n    prompt: STEP-TASK\n",
			}})
		}
		return textReply("root done")
	})

	r := toolResults(turnRequest(t, gw, "ROOT-TASK", 1))
	if countContaining(r, `"wf"`) != 1 || countContaining(r, `"other"`) != 1 || countContaining(r, "conflict") != 1 {
		t.Fatalf("root turn-1 results lack a conflict error naming both workflows: %q", r)
	}
	if n := len(gw.callsFor("STEP-TASK")); n != 0 {
		t.Fatalf("step requests = %d, want 0", n)
	}
	for _, v := range tree.SnapshotAll() {
		if v.Label == "pipeline p" {
			t.Fatal("a conflicting request created a coordinator node")
		}
	}
}
