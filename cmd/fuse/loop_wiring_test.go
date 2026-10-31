package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/tools"
)

// built is one stub child-builder observation: the child node and the registry
// loopWiring.childTools produced for it.
type built struct {
	node  *agent.AgentNode
	reg   *tools.Registry
	model string
	err   error
}

// wiringHarness drives loopWiring the way a binding does (change 0091): a real
// tree, one blackboard and one workflow scope, with a stub child builder that
// calls childTools and records the resulting registry instead of running a model.
type wiringHarness struct {
	t    *testing.T
	cfg  config.Config
	base *tools.Registry
	tree *agent.AgentTree
	root *agent.AgentNode
	w    loopWiring

	// reply, when set, overrides the stub child's result text (a synthesis child).
	reply func(opts agent.SpawnOpts) string

	mu   sync.Mutex
	seen []built
}

func newWiringHarness(t *testing.T) *wiringHarness {
	t.Helper()
	return newWiringHarnessN(t, 4)
}

// newWiringHarnessN is newWiringHarness with a max_concurrent of slots.
func newWiringHarnessN(t *testing.T, slots int) *wiringHarness {
	t.Helper()
	cfg := config.Default()
	cfg.Workflows = scopeWF()
	tree := agent.NewAgentTreeWithConcurrency("m", "m", slots)
	h := &wiringHarness{
		t:    t,
		cfg:  cfg,
		base: defaultToolRegistry(nil, cfg.Research, nil),
		tree: tree,
		root: tree.Node(tree.RootID()),
	}
	scope := newWorkflowScope(tree, cfg.Workflows)
	h.w = loopWiring{
		cfg:         cfg,
		tree:        tree,
		bb:          agent.NewBlackboard(tree),
		scope:       scope,
		makeSpawner: h.makeSpawner,
	}
	return h
}

func (h *wiringHarness) builder(_ context.Context, opts agent.SpawnOpts, node *agent.AgentNode, _ *agent.AgentTree) (string, error) {
	reg, model, err := h.w.childTools(h.base, opts, node)
	h.mu.Lock()
	h.seen = append(h.seen, built{node: node, reg: reg, model: model, err: err})
	h.mu.Unlock()
	if err != nil {
		return "", err
	}
	if h.reply != nil {
		if out := h.reply(opts); out != "" {
			return out, nil
		}
	}
	return "ok", nil
}

// makeSpawner mirrors a binding's makeSpawner: the scope's lazy backstop plus the
// shared child builder.
func (h *wiringHarness) makeSpawner(parent *agent.AgentNode, depth int) *agent.Spawner {
	return agent.NewSpawner(
		agent.WithTree(h.tree),
		agent.WithNode(parent),
		agent.WithSpawnDepth(depth),
		agent.WithSpawnBackstop(h.w.scope.backstop(parent)),
		agent.WithChildBuilder(h.builder),
	)
}

// observed returns the builder observation for nodeID.
func (h *wiringHarness) observed(nodeID string) built {
	h.t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, b := range h.seen {
		if b.node.ID == nodeID {
			return b
		}
	}
	h.t.Fatalf("child builder never ran for node %s", nodeID)
	return built{}
}

// spawn runs one child of parent to completion and returns its observation.
func (h *wiringHarness) spawn(parent *agent.AgentNode, opts agent.SpawnOpts) built {
	h.t.Helper()
	hd, err := h.makeSpawner(parent, parent.Depth).Spawn(context.Background(), opts)
	if err != nil {
		h.t.Fatalf("Spawn(%+v): %v", opts, err)
	}
	hd.Wait()
	b := h.observed(hd.NodeID)
	if b.err != nil {
		h.t.Fatalf("childTools(%+v): %v", opts, b.err)
	}
	return b
}

// lead roots wf on a root-level child and returns its observation.
func (h *wiringHarness) lead() built {
	h.t.Helper()
	return h.spawn(h.root, agent.SpawnOpts{Label: "lead", Task: "t", Workflow: "wf"})
}

// schemaNames returns reg's tool names, sorted.
func schemaNames(reg *tools.Registry) []string {
	var out []string
	for _, s := range reg.Schemas() {
		out = append(out, s.Name)
	}
	sort.Strings(out)
	return out
}

// toolProps returns the properties map of reg's tool name, or nil when absent.
func toolProps(t *testing.T, reg *tools.Registry, name string) map[string]any {
	t.Helper()
	for _, s := range reg.Schemas() {
		if s.Name != name {
			continue
		}
		props, ok := s.Parameters["properties"].(map[string]any)
		if !ok {
			t.Fatalf("%s parameters have no properties map", name)
		}
		return props
	}
	return nil
}

var blackboardNames = []string{"blackboard_delete", "blackboard_keys", "blackboard_read", "blackboard_wait", "blackboard_write"}

func TestChildToolsWorkerBoundSchemaIsAllowlistPlusBlackboard(t *testing.T) {
	h := newWiringHarness(t)
	lead := h.lead()
	reader := h.spawn(lead.node, agent.SpawnOpts{Label: "r", Task: "t", Worker: "reader"})

	want := append(append([]string(nil), blackboardNames...), "read_file")
	sort.Strings(want)
	if got := schemaNames(reader.reg); !reflect.DeepEqual(got, want) {
		t.Fatalf("worker-bound schema = %v, want %v", got, want)
	}
	if reader.model != "pin" {
		t.Fatalf("worker model pin = %q, want pin", reader.model)
	}
}

func TestChildToolsFreeformKeepsTodayShape(t *testing.T) {
	h := newWiringHarness(t)
	free := h.spawn(h.root, agent.SpawnOpts{Label: "free", Task: "t"})

	names := schemaNames(free.reg)
	for _, n := range append([]string{"spawn_agent", "pipeline_run"}, blackboardNames...) {
		if !contains(names, n) {
			t.Fatalf("freeform schema %v lacks %s", names, n)
		}
	}
	if _, ok := toolProps(t, free.reg, "spawn_agent")["workflow"]; !ok {
		t.Fatal("freeform spawn_agent lacks a workflow property")
	}
	if _, ok := toolProps(t, free.reg, "pipeline_run")["workflow"]; !ok {
		t.Fatal("freeform pipeline_run lacks a workflow property")
	}
	if free.model != "" {
		t.Fatalf("freeform model pin = %q, want none", free.model)
	}

	narrow := h.spawn(h.root, agent.SpawnOpts{Label: "narrow", Task: "t", Tools: []string{"read_file"}})
	if got := schemaNames(narrow.reg); !reflect.DeepEqual(got, []string{"read_file"}) {
		t.Fatalf("subset schema = %v, want [read_file]", got)
	}
}

func TestChildToolsInsideWorkflowAdvertisesWorkers(t *testing.T) {
	h := newWiringHarness(t)
	lead := h.lead()

	sp := toolProps(t, lead.reg, "spawn_agent")
	if sp == nil {
		t.Fatal("lead registry lacks spawn_agent")
	}
	if _, ok := sp["workflow"]; ok {
		t.Fatal("lead spawn_agent advertises workflow inside a workflow")
	}
	w, ok := sp["worker"].(map[string]any)
	if !ok {
		t.Fatal("lead spawn_agent lacks a worker property")
	}
	if got := w["enum"]; !reflect.DeepEqual(got, []any{"reader"}) {
		t.Fatalf("worker enum = %#v, want [reader]", got)
	}
	pr := toolProps(t, lead.reg, "pipeline_run")
	if pr == nil {
		t.Fatal("lead registry lacks pipeline_run")
	}
	if _, ok := pr["workflow"]; ok {
		t.Fatal("lead pipeline_run advertises workflow inside a workflow")
	}
}

func TestRootToolsRegistersScopedTools(t *testing.T) {
	h := newWiringHarness(t)
	reg := defaultToolRegistry(nil, h.cfg.Research, nil)
	h.w.rootTools(reg, h.root)

	names := schemaNames(reg)
	for _, n := range append([]string{"spawn_agent", "pipeline_run"}, blackboardNames...) {
		if !contains(names, n) {
			t.Fatalf("root schema %v lacks %s", names, n)
		}
	}
	if _, ok := toolProps(t, reg, "spawn_agent")["workflow"]; !ok {
		t.Fatal("root spawn_agent lacks a workflow property")
	}
	if _, ok := toolProps(t, reg, "pipeline_run")["workflow"]; !ok {
		t.Fatal("root pipeline_run lacks a workflow property")
	}
}

// nodesLabelled returns every tree node labelled label.
func nodesLabelled(tree *agent.AgentTree, label string) []agent.NodeView {
	var out []agent.NodeView
	for _, v := range tree.SnapshotAll() {
		if v.Label == label {
			out = append(out, v)
		}
	}
	return out
}

func TestRunnerRunsStepsUnderCoordinatorRoot(t *testing.T) {
	h := newWiringHarness(t)
	ctx := context.Background()
	var stepID string
	fn := func(ctx context.Context, steps *agent.Spawner) (string, error) {
		hd, err := steps.Spawn(ctx, agent.SpawnOpts{Label: "s1", Task: "t", Worker: "reader"})
		if err != nil {
			return "", err
		}
		d := hd.Wait()
		stepID = hd.NodeID
		return "steps done", d.Err
	}

	run := h.w.scope.runner(h.makeSpawner, h.root)
	out, err := run(ctx, "pipeline p", "wf", fn)
	if err != nil {
		t.Fatalf("runner: %v", err)
	}
	if out != "steps done" {
		t.Fatalf("runner result = %q, want the fn result", out)
	}
	coords := nodesLabelled(h.tree, "pipeline p")
	if len(coords) != 1 {
		t.Fatalf("coordinator nodes = %d, want 1", len(coords))
	}
	coord := coords[0]
	if coord.WorkflowRoot != "wf" || coord.ParentID != h.root.ID {
		t.Fatalf("coordinator = %+v, want WorkflowRoot wf under the root", coord)
	}
	if got := h.tree.WorkflowRootOf(stepID); got != coord.ID {
		t.Fatalf("step WorkflowRootOf = %q, want coordinator %q", got, coord.ID)
	}
	if pool, ok := h.tree.Scheduler().Pool(coord.ID); !ok || pool.Total != 2 {
		t.Fatalf("coordinator pool = %+v (registered %v), want Total 2", pool, ok)
	}
	want := append(append([]string(nil), blackboardNames...), "read_file")
	sort.Strings(want)
	if got := schemaNames(h.observed(stepID).reg); !reflect.DeepEqual(got, want) {
		t.Fatalf("step schema = %v, want %v", got, want)
	}

	// Nested: a runner at a node inside a workflow is refused.
	lead := h.lead()
	if _, err := h.w.scope.runner(h.makeSpawner, lead.node)(ctx, "pipeline q", "wf", fn); !errors.Is(err, agent.ErrNestedWorkflow) {
		t.Fatalf("nested runner err = %v, want ErrNestedWorkflow", err)
	}

	// Unknown: refused before any coordinator exists, fn never runs.
	called := false
	_, err = run(ctx, "pipeline r", "nope", func(context.Context, *agent.Spawner) (string, error) {
		called = true
		return "", nil
	})
	if err == nil || !strings.Contains(err.Error(), `unknown workflow "nope"`) {
		t.Fatalf("unknown runner err = %v, want unknown workflow", err)
	}
	if called {
		t.Fatal("runner called fn for an unknown workflow")
	}
	if n := len(nodesLabelled(h.tree, "pipeline r")); n != 0 {
		t.Fatalf("unknown workflow created %d coordinator nodes", n)
	}
}

// TestRunnerYieldsCoordinatorSlot pins the coordinator's slot yield (learning
// slot-cap-yield-while-blocked-on-children): with one slot, the coordinator
// holds it on admission, so its step can only run if the coordinator yields.
func TestRunnerYieldsCoordinatorSlot(t *testing.T) {
	h := newWiringHarnessN(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	run := h.w.scope.runner(h.makeSpawner, h.root)
	out, err := run(ctx, "pipeline p", "wf", func(ctx context.Context, steps *agent.Spawner) (string, error) {
		hd, err := steps.Spawn(ctx, agent.SpawnOpts{Label: "s1", Task: "t", Worker: "reader"})
		if err != nil {
			return "", err
		}
		d := hd.Wait()
		return d.Result, d.Err
	})
	if err != nil || out != "ok" {
		t.Fatalf("one-slot runner = %q, %v; want the step result (coordinator must yield its slot)", out, err)
	}
}

const wiringPipelineDef = "name: p\nsteps:\n  - name: s1\n    worker: reader\n    prompt: hi\n"

func TestPipelineRunFnWorkflowUsesRunner(t *testing.T) {
	ctx := context.Background()

	h := newWiringHarness(t)
	sched := h.tree.Scheduler()
	sp := h.makeSpawner(h.root, 0)
	runFn := makePipelineRunFn(sp, h.w.bb, sched, h.root, h.w.scope.runner(h.makeSpawner, h.root))
	line, err := runFn(ctx, []byte(wiringPipelineDef), "wf")
	if err != nil {
		t.Fatalf("workflow pipeline: %v", err)
	}
	if line != "pipeline p: completed" {
		t.Fatalf("workflow pipeline line = %q, want completed", line)
	}
	coords := nodesLabelled(h.tree, "pipeline p")
	if len(coords) != 1 || coords[0].WorkflowRoot != "wf" {
		t.Fatalf("workflow pipeline coordinators = %+v, want one rooting wf", coords)
	}

	plain := newWiringHarness(t)
	plainFn := makePipelineRunFn(plain.makeSpawner(plain.root, 0), plain.w.bb, plain.tree.Scheduler(), plain.root,
		plain.w.scope.runner(plain.makeSpawner, plain.root))
	line, err = plainFn(ctx, []byte(wiringPipelineDef), "")
	if err != nil || line != "pipeline p: completed" {
		t.Fatalf("plain pipeline = %q, %v; want completed", line, err)
	}
	if n := len(nodesLabelled(plain.tree, "pipeline p")); n != 0 {
		t.Fatalf("plain pipeline created %d coordinator nodes, want 0", n)
	}

	none := makePipelineRunFn(sp, h.w.bb, sched, h.root, nil)
	if _, err := none(ctx, []byte(wiringPipelineDef), "wf"); err == nil || !strings.Contains(err.Error(), "not available") {
		t.Fatalf("nil runner err = %v, want not available", err)
	}
}

func TestPipelineSynthFnWorkflowUsesRunner(t *testing.T) {
	ctx := context.Background()
	h := newWiringHarness(t)
	var synthTask string
	h.reply = func(opts agent.SpawnOpts) string {
		if opts.Label != "synthesize" {
			return ""
		}
		synthTask = opts.SystemPrompt
		// No workflow field: the argument supplies it.
		return `{"name":"p","steps":[{"name":"s1","worker":"reader","prompt":"hi"}]}`
	}
	sched := h.tree.Scheduler()
	sp := h.makeSpawner(h.root, 0)
	run := h.w.scope.runner(h.makeSpawner, h.root)
	synth := makePipelineSynthFn(sp, h.w.bb, sched, h.root, h.cfg, []string{"other", "reader"}, nil, nil, run)

	if _, err := synth(ctx, "goal", false, "nope"); err == nil || err.Error() != `unknown workflow "nope" (available: wf)` {
		t.Fatalf("unknown workflow err = %v, want the scope's refusal text", err)
	}
	if n := len(nodesLabelled(h.tree, "synthesize")); n != 0 {
		t.Fatalf("unknown workflow spawned %d synthesis children, want 0", n)
	}

	line, err := synth(ctx, "goal", false, "wf")
	if err != nil {
		t.Fatalf("workflow synthesis: %v", err)
	}
	if line != "pipeline p: completed" {
		t.Fatalf("workflow synthesis line = %q, want completed", line)
	}
	if !strings.Contains(synthTask, "Available worker types: reader\n") {
		t.Fatalf("synthesis palette not narrowed to wf's workers: %q", synthTask)
	}
	coords := nodesLabelled(h.tree, "pipeline p")
	if len(coords) != 1 || coords[0].WorkflowRoot != "wf" {
		t.Fatalf("workflow synthesis coordinators = %+v, want one rooting wf", coords)
	}
	entry, ok := h.w.bb.Get(pipelinePlanKey)
	if !ok {
		t.Fatal("pipeline.plan was not published")
	}
	b, _ := json.Marshal(entry.Value)
	if !strings.Contains(string(b), `"workflow":"wf"`) {
		t.Fatalf("published plan %s does not record workflow wf", b)
	}

	if _, err := makePipelineSynthFn(sp, h.w.bb, sched, h.root, h.cfg, nil, nil, nil, nil)(ctx, "goal", false, "wf"); err == nil || !strings.Contains(err.Error(), "not available") {
		t.Fatalf("nil runner synthesis err = %v, want not available", err)
	}
}

// TestPipelineSynthFnRefusesNestedWorkflowBeforeSynthesis pins that a workflow
// synthesis requested from inside a workflow subtree is refused up front, like
// the authored path: no synthesis child is spawned and no pipeline.plan is
// published.
func TestPipelineSynthFnRefusesNestedWorkflowBeforeSynthesis(t *testing.T) {
	ctx := context.Background()
	h := newWiringHarness(t)
	h.reply = func(opts agent.SpawnOpts) string {
		if opts.Label == "synthesize" {
			return `{"name":"p","steps":[{"name":"s1","worker":"reader","prompt":"hi"}]}`
		}
		return ""
	}
	lead := h.lead()
	sp := h.makeSpawner(lead.node, lead.node.Depth)
	run := h.w.scope.runner(h.makeSpawner, lead.node)
	synth := makePipelineSynthFn(sp, h.w.bb, h.tree.Scheduler(), lead.node, h.cfg, []string{"reader"}, nil, nil, run)

	if _, err := synth(ctx, "goal", false, "wf"); !errors.Is(err, agent.ErrNestedWorkflow) {
		t.Fatalf("nested synthesis err = %v, want ErrNestedWorkflow", err)
	}
	if n := len(nodesLabelled(h.tree, "synthesize")); n != 0 {
		t.Fatalf("nested synthesis spawned %d synthesis children, want 0", n)
	}
	if _, ok := h.w.bb.Get(pipelinePlanKey); ok {
		t.Fatal("nested synthesis published pipeline.plan")
	}
}

// deepNode spawns a plain (workflow-free) chain under the root until a node at
// depth exists, and returns it.
func (h *wiringHarness) deepNode(depth int) *agent.AgentNode {
	h.t.Helper()
	n := h.root
	for n.Depth < depth {
		n = h.spawn(n, agent.SpawnOpts{Label: "hop", Task: "t"}).node
	}
	return n
}

// TestRunnerRefusesTooDeepWorkflowPipeline pins the up-front depth refusal: a
// workflow pipeline whose steps would sit beyond agent.MaxDepth creates no
// coordinator and never runs fn, and pipeline_run there omits `workflow`.
func TestRunnerRefusesTooDeepWorkflowPipeline(t *testing.T) {
	ctx := context.Background()
	h := newWiringHarness(t)
	deep := h.deepNode(agent.MaxDepth - 1)
	called := false
	_, err := h.w.scope.runner(h.makeSpawner, deep)(ctx, "pipeline deep", "wf", func(context.Context, *agent.Spawner) (string, error) {
		called = true
		return "", nil
	})
	if !errors.Is(err, agent.ErrMaxDepthExceeded) || !strings.Contains(err.Error(), `workflow "wf"`) {
		t.Fatalf("too-deep runner err = %v, want a workflow ErrMaxDepthExceeded refusal", err)
	}
	if called {
		t.Fatal("too-deep runner ran fn")
	}
	if n := len(nodesLabelled(h.tree, "pipeline deep")); n != 0 {
		t.Fatalf("too-deep runner created %d coordinator nodes, want 0", n)
	}
	reg := tools.NewRegistry()
	h.w.wirePipeline(reg, deep, nil)
	if _, ok := toolProps(t, reg, "pipeline_run")["workflow"]; ok {
		t.Fatal("pipeline_run at a too-deep node advertises a workflow property")
	}

	// One level shallower the steps fit at MaxDepth: the runner admits it.
	ok := h.deepNode(agent.MaxDepth - 2)
	out, err := h.w.scope.runner(h.makeSpawner, ok)(ctx, "pipeline fits", "wf", func(ctx context.Context, steps *agent.Spawner) (string, error) {
		hd, err := steps.Spawn(ctx, agent.SpawnOpts{Label: "s1", Task: "t", Worker: "reader"})
		if err != nil {
			return "", err
		}
		d := hd.Wait()
		return d.Result, d.Err
	})
	if err != nil || out != "ok" {
		t.Fatalf("fitting runner = %q, %v; want the step result", out, err)
	}
	reg = tools.NewRegistry()
	h.w.wirePipeline(reg, ok, nil)
	if _, has := toolProps(t, reg, "pipeline_run")["workflow"]; !has {
		t.Fatal("pipeline_run at a fitting depth lacks the workflow property")
	}
}
