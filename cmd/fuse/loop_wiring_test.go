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
	plan  childPlan
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
	return newWiringHarnessCfg(t, slots, nil)
}

// newToolLessWiringHarness is newWiringHarness plus a tool-less judge role and
// a tool-less drafter worker in wf. They live here, not in the shared
// fixtures, so the role and worker enums other tests pin stay as they are.
func newToolLessWiringHarness(t *testing.T) *wiringHarness {
	t.Helper()
	return newWiringHarnessCfg(t, 4, func(cfg *config.Config) {
		cfg.Roles["judge"] = config.RoleConfig{Description: "Judges.", NoTools: true}
		wf := cfg.Workflows["wf"]
		wf.Workers["drafter"] = config.WorkerConfig{NoTools: true}
		cfg.Workflows["wf"] = wf
	})
}

// newWiringHarnessCfg builds the wiring harness, letting edit adjust the
// fixture config before the scope is built from it. A nil edit is a no-op.
func newWiringHarnessCfg(t *testing.T, slots int, edit func(*config.Config)) *wiringHarness {
	t.Helper()
	cfg := config.Default()
	cfg.Workflows = scopeWF()
	cfg.Roles = scopeRoles()
	writer := cfg.Roles["writer"]
	writer.Instructions = "WRITER-BRIEF"
	cfg.Roles["writer"] = writer
	if edit != nil {
		edit(&cfg)
	}
	tree := agent.NewAgentTreeWithConcurrency("m", "m", slots)
	h := &wiringHarness{
		t:    t,
		cfg:  cfg,
		base: defaultToolRegistry(nil, cfg.Research, nil),
		tree: tree,
		root: tree.Node(tree.RootID()),
	}
	scope := newWorkflowScope(tree, cfg.Workflows, cfg.Roles)
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
	reg, plan, err := h.w.childTools(h.base, opts, node)
	h.mu.Lock()
	h.seen = append(h.seen, built{node: node, reg: reg, model: plan.model, plan: plan, err: err})
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

// TestRoleChildToolsReviewerBound: a role with a non-empty tool set is the
// child's whole tool set plus the five blackboard tools (bound), with no
// spawn_agent or pipeline_run, and its plan carries the brief and model pin
// every child builder applies (change 0114).
func TestRoleChildToolsReviewerBound(t *testing.T) {
	h := newWiringHarness(t)
	rev := h.spawn(h.root, agent.SpawnOpts{Label: "rev", Task: "t", Role: "reviewer"})

	want := append(append([]string(nil), blackboardNames...), "grep", "read_file")
	sort.Strings(want)
	if got := schemaNames(rev.reg); !reflect.DeepEqual(got, want) {
		t.Fatalf("reviewer schema = %v, want %v", got, want)
	}
	if rev.plan.model != "rmodel" || rev.model != "rmodel" {
		t.Fatalf("reviewer model pin = %q (plan %q), want rmodel", rev.model, rev.plan.model)
	}
	if rev.plan.instructions != "REVIEWER-BRIEF" {
		t.Fatalf("reviewer instructions = %q, want REVIEWER-BRIEF", rev.plan.instructions)
	}
	if rev.node.Role != "reviewer" {
		t.Fatalf("reviewer node role = %q, want reviewer", rev.node.Role)
	}
}

// TestRoleChildToolsWriterInherits: a role with no tool set inherits exactly
// what a plain child gets under the same binding (nothing added, nothing
// withheld), and its plan carries the brief with no model pin (change 0114).
func TestRoleChildToolsWriterInherits(t *testing.T) {
	h := newWiringHarness(t)
	plain := h.spawn(h.root, agent.SpawnOpts{Label: "plain", Task: "t"})
	writer := h.spawn(h.root, agent.SpawnOpts{Label: "writer", Task: "t", Role: "writer"})

	if got, want := schemaNames(writer.reg), schemaNames(plain.reg); !reflect.DeepEqual(got, want) {
		t.Fatalf("writer schema = %v, want the plain child's %v", got, want)
	}
	if writer.plan.instructions != "WRITER-BRIEF" {
		t.Fatalf("writer instructions = %q, want WRITER-BRIEF", writer.plan.instructions)
	}
	if writer.plan.model != "" {
		t.Fatalf("writer model pin = %q, want none", writer.plan.model)
	}
	if plain.plan.instructions != "" || plain.node.Role != "" {
		t.Fatalf("plain child plan = %+v, want no brief and no role", plain.plan)
	}
}

// TestRolePlanCarriesPinWhenCallNamesModel: even when the spawn call names its
// own model, childTools still hands the site the role's pin as plan.model; it
// is the site that applies opts.ModelID ahead of plan.model (and the binding
// alias after), and that end-to-end precedence is covered by role_parity_test.go.
func TestRolePlanCarriesPinWhenCallNamesModel(t *testing.T) {
	h := newWiringHarness(t)
	rev := h.spawn(h.root, agent.SpawnOpts{Label: "rev", Task: "t", Role: "reviewer", ModelID: "call"})
	if rev.plan.model != "rmodel" {
		t.Fatalf("plan.model = %q, want the role pin rmodel (the site applies opts.ModelID first)", rev.plan.model)
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

// TestChildToolsToolLessRole: a tool-less role child gets an empty registry:
// no tools, no blackboard, no spawn_agent and no pipeline_run, and its plan is
// marked none so every child builder composes the tool-less prompt.
func TestChildToolsToolLessRole(t *testing.T) {
	h := newToolLessWiringHarness(t)
	j := h.spawn(h.root, agent.SpawnOpts{Label: "j", Task: "t", Role: "judge"})

	if n := len(j.reg.Tools()); n != 0 {
		t.Fatalf("tool-less role registry has %d tools %v, want none", n, schemaNames(j.reg))
	}
	if !j.plan.none {
		t.Fatalf("tool-less role plan = %+v, want none", j.plan)
	}
	if j.plan.bound || j.plan.tools != nil {
		t.Fatalf("tool-less role plan = %+v, want unbound with no tool list", j.plan)
	}
	if !j.plan.agentConfig(h.cfg).NoTools {
		t.Fatal("tool-less role agentConfig does not set NoTools")
	}
	if h.cfg.NoTools {
		t.Fatal("agentConfig mutated the loop's own config")
	}

	// The ordinary children beside it keep their tools.
	rev := h.spawn(h.root, agent.SpawnOpts{Label: "rev", Task: "t", Role: "reviewer"})
	if rev.plan.none || rev.plan.agentConfig(h.cfg).NoTools || len(rev.reg.Tools()) == 0 {
		t.Fatalf("reviewer plan = %+v with %v, want an ordinary bound child", rev.plan, schemaNames(rev.reg))
	}
	free := h.spawn(h.root, agent.SpawnOpts{Label: "free", Task: "t"})
	if free.plan.none || !contains(schemaNames(free.reg), "spawn_agent") {
		t.Fatalf("freeform plan = %+v with %v, want an ordinary child with spawn_agent", free.plan, schemaNames(free.reg))
	}
}

// TestChildToolsToolLessWorker: a tool-less worker child gets an empty
// registry, while a tooled worker in the same workflow keeps its allowlist plus
// the five blackboard tools.
func TestChildToolsToolLessWorker(t *testing.T) {
	h := newToolLessWiringHarness(t)
	lead := h.lead()
	d := h.spawn(lead.node, agent.SpawnOpts{Label: "d", Task: "t", Worker: "drafter"})

	if n := len(d.reg.Tools()); n != 0 {
		t.Fatalf("tool-less worker registry has %d tools %v, want none", n, schemaNames(d.reg))
	}
	if !d.plan.none || !d.plan.agentConfig(h.cfg).NoTools {
		t.Fatalf("tool-less worker plan = %+v, want none", d.plan)
	}

	r := h.spawn(lead.node, agent.SpawnOpts{Label: "r", Task: "t", Worker: "reader"})
	if got := schemaNames(r.reg); !reflect.DeepEqual(got, readerSchema) {
		t.Fatalf("reader schema = %v, want %v", got, readerSchema)
	}
	if r.plan.none {
		t.Fatalf("reader plan = %+v, want not none", r.plan)
	}
}

// TestToolLessRoleSpawnRefusesTools: the root's spawn_agent refuses a tool
// list for a tool-less role before any node exists.
func TestToolLessRoleSpawnRefusesTools(t *testing.T) {
	h := newToolLessWiringHarness(t)
	reg := tools.NewRegistry()
	h.w.rootTools(reg, h.root)
	before := len(h.tree.SnapshotAll())

	res := reg.Execute(context.Background(), "spawn_agent",
		argsJSON(map[string]any{"label": "j", "task": "t", "role": "judge", "tools": []string{"read_file"}}))
	if !res.IsError || !strings.Contains(res.Output, `role "judge" permits no tools`) {
		t.Fatalf("spawn_agent = %+v, want a permits-no-tools refusal", res)
	}
	if after := len(h.tree.SnapshotAll()); after != before {
		t.Fatalf("tree has %d nodes after the refusal, want %d (no node created)", after, before)
	}
}

// TestToolLessRoleOptionDescription: the role enum marks a tool-less role as
// answering in plain text and leaves every other role's description alone.
func TestToolLessRoleOptionDescription(t *testing.T) {
	h := newToolLessWiringHarness(t)
	got := map[string]string{}
	for _, o := range h.w.scope.roleOptions(h.root.ID) {
		got[o.Name] = o.Description
	}
	if want := "Judges. (no tools: answers in plain text)"; got["judge"] != want {
		t.Fatalf("judge description = %q, want %q", got["judge"], want)
	}
	for name, role := range h.cfg.Roles {
		if role.NoTools {
			continue
		}
		if got[name] != role.Description {
			t.Fatalf("%s description = %q, want it unchanged %q", name, got[name], role.Description)
		}
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

// wiringPlainPipelineDef names no worker: outside a workflow a worker step is
// refused (agent.ErrWorkerOutsideWorkflow), so the plain path runs freeform steps.
const wiringPlainPipelineDef = "name: p\nsteps:\n  - name: s1\n    prompt: hi\n"

func TestPipelineRunFnWorkflowUsesRunner(t *testing.T) {
	ctx := context.Background()

	h := newWiringHarness(t)
	sched := h.tree.Scheduler()
	sp := h.makeSpawner(h.root, 0)
	runFn := makePipelineRunFn(sp, h.w.bb, sched, h.root, h.w.scope.runner(h.makeSpawner, h.root), h.w.scope.governing, nil)
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
		plain.w.scope.runner(plain.makeSpawner, plain.root), plain.w.scope.governing, nil)
	line, err = plainFn(ctx, []byte(wiringPlainPipelineDef), "")
	if err != nil || line != "pipeline p: completed" {
		t.Fatalf("plain pipeline = %q, %v; want completed", line, err)
	}
	if n := len(nodesLabelled(plain.tree, "pipeline p")); n != 0 {
		t.Fatalf("plain pipeline created %d coordinator nodes, want 0", n)
	}

	none := makePipelineRunFn(sp, h.w.bb, sched, h.root, nil, nil, nil)
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
	synth := makePipelineSynthFn(sp, h.w.bb, sched, h.root, h.cfg, nil, nil, run, h.w.scope.governing)

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
	synth := makePipelineSynthFn(sp, h.w.bb, h.tree.Scheduler(), lead.node, h.cfg, nil, nil, run, h.w.scope.governing)

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

// TestPipelineRunFnRefusesWorkerStepsOutsideWorkflow pins that an authored
// pipeline whose steps name a worker is refused when no workflow will govern
// them (no argument, no definition field, caller outside every workflow
// subtree), before any step spawns, and that every governed form still runs.
func TestPipelineRunFnRefusesWorkerStepsOutsideWorkflow(t *testing.T) {
	ctx := context.Background()
	h := newWiringHarness(t)
	runFn := makePipelineRunFn(h.makeSpawner(h.root, 0), h.w.bb, h.tree.Scheduler(), h.root,
		h.w.scope.runner(h.makeSpawner, h.root), h.w.scope.governing, nil)

	before := len(h.tree.SnapshotAll())
	slots := h.tree.Scheduler().Snapshot().SlotsInUse
	_, err := runFn(ctx, []byte(wiringPipelineDef), "")
	if !errors.Is(err, agent.ErrWorkerOutsideWorkflow) {
		t.Fatalf("outside-workflow worker pipeline err = %v, want ErrWorkerOutsideWorkflow", err)
	}
	if !strings.Contains(err.Error(), `step "s1" names worker "reader"`) || !strings.Contains(err.Error(), "pass workflow") {
		t.Fatalf("refusal %q does not name the step and worker and say to pass workflow", err)
	}
	if n := len(h.tree.SnapshotAll()); n != before {
		t.Fatalf("refused pipeline created %d nodes, want 0", n-before)
	}
	h.mu.Lock()
	seen := len(h.seen)
	h.mu.Unlock()
	if seen != 0 {
		t.Fatalf("refused pipeline reached the child builder %d times, want 0", seen)
	}
	if got := h.tree.Scheduler().Snapshot().SlotsInUse; got != slots {
		t.Fatalf("refused pipeline left SlotsInUse %d, want %d", got, slots)
	}

	if line, err := runFn(ctx, []byte(wiringPipelineDef), "wf"); err != nil || line != "pipeline p: completed" {
		t.Fatalf("workflow argument pipeline = %q, %v; want completed", line, err)
	}
	const defWithWorkflow = "name: p\nworkflow: wf\nsteps:\n  - name: s1\n    worker: reader\n    prompt: hi\n"
	if line, err := runFn(ctx, []byte(defWithWorkflow), ""); err != nil || line != "pipeline p: completed" {
		t.Fatalf("definition-field pipeline = %q, %v; want completed", line, err)
	}

	// Inside the wf subtree the governing workflow binds the worker.
	sub := newWiringHarness(t)
	lead := sub.lead()
	subFn := makePipelineRunFn(sub.makeSpawner(lead.node, lead.node.Depth), sub.w.bb, sub.tree.Scheduler(), lead.node,
		sub.w.scope.runner(sub.makeSpawner, lead.node), sub.w.scope.governing, nil)
	if line, err := subFn(ctx, []byte(wiringPipelineDef), ""); err != nil || line != "pipeline p: completed" {
		t.Fatalf("in-subtree worker pipeline = %q, %v; want completed", line, err)
	}
	steps := nodesLabelled(sub.tree, "s1")
	if len(steps) != 1 {
		t.Fatalf("in-subtree pipeline ran %d s1 steps, want 1", len(steps))
	}
	if got := schemaNames(sub.observed(steps[0].ID).reg); !containsAll(got, "read_file") || containsAll(got, "write_file") {
		t.Fatalf("in-subtree step tools = %v, want the reader allowlist (read_file, no write_file)", got)
	}

	// A nil lookup is a context with no workflow scope: refused like outside.
	nilFn := makePipelineRunFn(h.makeSpawner(h.root, 0), h.w.bb, h.tree.Scheduler(), h.root,
		h.w.scope.runner(h.makeSpawner, h.root), nil, nil)
	if _, err := nilFn(ctx, []byte(wiringPipelineDef), ""); !errors.Is(err, agent.ErrWorkerOutsideWorkflow) {
		t.Fatalf("nil lookup err = %v, want ErrWorkerOutsideWorkflow", err)
	}
}

// containsAll reports whether got holds every name in want.
func containsAll(got []string, want ...string) bool {
	have := map[string]bool{}
	for _, g := range got {
		have[g] = true
	}
	for _, w := range want {
		if !have[w] {
			return false
		}
	}
	return true
}

// TestPipelineSynthPaletteOutsideWorkflowOffersNoWorkers pins the per-call
// synthesis palette: no workers outside a workflow, the argument's workflow
// workers with one, and the governing workflow's workers inside a subtree.
func TestPipelineSynthPaletteOutsideWorkflowOffersNoWorkers(t *testing.T) {
	ctx := context.Background()
	h := newWiringHarness(t)
	h.cfg.Workflows["other"] = config.WorkflowConfig{Workers: map[string]config.WorkerConfig{"writer": {Tools: []string{"write_file"}}}}
	var prompt string
	h.reply = func(opts agent.SpawnOpts) string {
		if opts.Label != "synthesize" {
			return ""
		}
		prompt = opts.SystemPrompt
		return `{"name":"p","workflow":"wf","steps":[{"name":"s1","prompt":"hi"}]}`
	}
	sched := h.tree.Scheduler()
	synth := makePipelineSynthFn(h.makeSpawner(h.root, 0), h.w.bb, sched, h.root, h.cfg, nil, nil,
		h.w.scope.runner(h.makeSpawner, h.root), h.w.scope.governing)

	if _, err := synth(ctx, "goal", false, ""); err != nil {
		t.Fatalf("root synthesis: %v", err)
	}
	if strings.Contains(prompt, "Available worker types:") {
		t.Fatalf("root synthesis offered workers outside a workflow: %q", prompt)
	}

	prompt = ""
	if _, err := synth(ctx, "goal", false, "wf"); err != nil {
		t.Fatalf("wf synthesis: %v", err)
	}
	if !strings.Contains(prompt, "Available worker types: reader\n") || strings.Contains(synthWorkerLine(prompt), "writer") {
		t.Fatalf("wf synthesis palette = %q, want only reader", prompt)
	}

	prompt = ""
	lead := h.lead()
	sub := makePipelineSynthFn(h.makeSpawner(lead.node, lead.node.Depth), h.w.bb, sched, lead.node, h.cfg, nil, nil,
		h.w.scope.runner(h.makeSpawner, lead.node), h.w.scope.governing)
	// A synthesized plan naming wf inside the subtree is refused as nested
	// (unchanged behavior); only the palette matters here.
	_, _ = sub(ctx, "goal", false, "")
	if !strings.Contains(prompt, "Available worker types: reader\n") || strings.Contains(synthWorkerLine(prompt), "writer") {
		t.Fatalf("in-subtree synthesis palette = %q, want only reader", prompt)
	}
}

// synthWorkerLine is the synthesis prompt's worker palette line, or "" when it
// has none. The harness also defines a writer role, which the role palette
// lists, so a worker-palette check looks only at this line.
func synthWorkerLine(prompt string) string {
	for _, line := range strings.Split(prompt, "\n") {
		if strings.HasPrefix(line, "Available worker types:") {
			return line
		}
	}
	return ""
}

// TestPipelineSynthFnRefusesUnboundWorkerSteps pins that a synthesized plan whose
// steps name a worker no workflow will govern is refused before any step
// spawns, and that the caller's slot is given back.
func TestPipelineSynthFnRefusesUnboundWorkerSteps(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h := newWiringHarness(t)
	h.reply = func(opts agent.SpawnOpts) string {
		if opts.Label == "synthesize" {
			return `{"name":"p","workflow":"wf","steps":[{"name":"s1","worker":"reader","prompt":"hi"}]}`
		}
		return ""
	}
	synth := makePipelineSynthFn(h.makeSpawner(h.root, 0), h.w.bb, h.tree.Scheduler(), h.root, h.cfg, nil, nil, nil, nil)

	_, err := synth(ctx, "goal", false, "")
	if !errors.Is(err, agent.ErrWorkerOutsideWorkflow) || !strings.Contains(err.Error(), `step "s1"`) {
		t.Fatalf("unbound synthesized worker err = %v, want ErrWorkerOutsideWorkflow naming s1", err)
	}
	if n := len(nodesLabelled(h.tree, "s1")); n != 0 {
		t.Fatalf("refused synthesis spawned %d step children, want 0", n)
	}
	if n := len(nodesLabelled(h.tree, "synthesize")); n != 1 {
		t.Fatalf("synthesize nodes = %d, want 1", n)
	}
	if _, err := synth(ctx, "goal", false, ""); !errors.Is(err, agent.ErrWorkerOutsideWorkflow) {
		t.Fatalf("second call err = %v, want the same refusal (slot was given back)", err)
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

// TestPipelineRunFnRoleStepOutsideWorkflow (change 0115): a role step is not
// refused outside a workflow (roles are global); its child is built through
// prepareChild's role resolution, so it gets the reviewer's tools, model pin
// and brief, and the node records the role.
func TestPipelineRunFnRoleStepOutsideWorkflow(t *testing.T) {
	ctx := context.Background()
	h := newWiringHarness(t)
	runFn := makePipelineRunFn(h.makeSpawner(h.root, 0), h.w.bb, h.tree.Scheduler(), h.root,
		h.w.scope.runner(h.makeSpawner, h.root), h.w.scope.governing, h.w.scope)
	const def = "name: rp\nsteps:\n  - name: rev\n    role: reviewer\n    prompt: check\n"
	line, err := runFn(ctx, []byte(def), "")
	if err != nil || line != "pipeline rp: completed" {
		t.Fatalf("role pipeline = %q, %v; want completed", line, err)
	}
	steps := nodesLabelled(h.tree, "rev")
	if len(steps) != 1 || steps[0].Role != "reviewer" {
		t.Fatalf("rev steps = %+v, want one reviewer node", steps)
	}
	b := h.observed(steps[0].ID)
	want := append(append([]string(nil), blackboardNames...), "grep", "read_file")
	sort.Strings(want)
	if got := schemaNames(b.reg); !reflect.DeepEqual(got, want) {
		t.Fatalf("role step tools = %v, want %v", got, want)
	}
	if b.plan.model != "rmodel" || b.plan.instructions != "REVIEWER-BRIEF" {
		t.Fatalf("role step plan = %+v, want rmodel pin and REVIEWER-BRIEF", b.plan)
	}
}

// TestPipelineRunFnRoleStepUnderWorkflow: inside a workflow run the role step
// is a child of the coordinator and resolves against the workflow's offered
// roles (wf lists none, so every role is offered).
func TestPipelineRunFnRoleStepUnderWorkflow(t *testing.T) {
	ctx := context.Background()
	h := newWiringHarness(t)
	runFn := makePipelineRunFn(h.makeSpawner(h.root, 0), h.w.bb, h.tree.Scheduler(), h.root,
		h.w.scope.runner(h.makeSpawner, h.root), h.w.scope.governing, h.w.scope)
	const def = "name: rp\nsteps:\n  - name: wri\n    role: writer\n    prompt: draft\n"
	if line, err := runFn(ctx, []byte(def), "wf"); err != nil || line != "pipeline rp: completed" {
		t.Fatalf("workflow role pipeline = %q, %v; want completed", line, err)
	}
	steps := nodesLabelled(h.tree, "wri")
	if len(steps) != 1 || steps[0].Role != "writer" {
		t.Fatalf("wri steps = %+v, want one writer node", steps)
	}
	if got := h.observed(steps[0].ID).plan.instructions; got != "WRITER-BRIEF" {
		t.Fatalf("writer step instructions = %q, want WRITER-BRIEF", got)
	}
}

// TestPipelineRunFnRefusesBadRoleSteps: an unknown role and a role+worker step
// are parse errors, refused before any node exists.
func TestPipelineRunFnRefusesBadRoleSteps(t *testing.T) {
	ctx := context.Background()
	h := newWiringHarness(t)
	runFn := makePipelineRunFn(h.makeSpawner(h.root, 0), h.w.bb, h.tree.Scheduler(), h.root,
		h.w.scope.runner(h.makeSpawner, h.root), h.w.scope.governing, h.w.scope)
	before := len(h.tree.SnapshotAll())
	for def, want := range map[string]string{
		"name: p\nsteps:\n  - name: s1\n    role: nope\n    prompt: x\n":                         `parse: pipeline: step "s1": unknown role "nope" (available: reviewer, writer)`,
		"name: p\nsteps:\n  - name: s1\n    role: reviewer\n    worker: reader\n    prompt: x\n": `parse: pipeline: step "s1": role "reviewer" and worker "reader" cannot be combined; name one`,
	} {
		if _, err := runFn(ctx, []byte(def), "wf"); err == nil || err.Error() != want {
			t.Errorf("err = %v, want %q", err, want)
		}
	}
	if n := len(h.tree.SnapshotAll()); n != before {
		t.Fatalf("refused pipelines created %d nodes, want 0", n-before)
	}
	// With no roles defined, any role step is an unknown role.
	noRoles := makePipelineRunFn(h.makeSpawner(h.root, 0), h.w.bb, h.tree.Scheduler(), h.root, nil, nil, nil)
	if _, err := noRoles(ctx, []byte("name: p\nsteps:\n  - name: s1\n    role: reviewer\n    prompt: x\n"), ""); err == nil ||
		!strings.Contains(err.Error(), `unknown role "reviewer" (available: none configured)`) {
		t.Fatalf("no-roles err = %v, want unknown role", err)
	}
}

// restrictWFToWriter narrows h's wf (shared with its scope) to the writer role.
func restrictWFToWriter(h *wiringHarness) {
	wf := h.cfg.Workflows["wf"]
	wf.Roles = []string{"writer"}
	h.cfg.Workflows["wf"] = wf
}

// TestPipelineRunFnRefusesUnofferedRoleSteps: a step naming a defined role the
// steps' workflow does not offer (through the argument, the definition field,
// or the governing workflow) is refused before the slot yield and any spawn,
// with the reason, rather than failing mid-run at that step.
func TestPipelineRunFnRefusesUnofferedRoleSteps(t *testing.T) {
	ctx := context.Background()
	h := newWiringHarness(t)
	restrictWFToWriter(h)
	runFn := makePipelineRunFn(h.makeSpawner(h.root, 0), h.w.bb, h.tree.Scheduler(), h.root,
		h.w.scope.runner(h.makeSpawner, h.root), h.w.scope.governing, h.w.scope)
	const def = "name: p\nsteps:\n  - name: up\n    role: writer\n    prompt: x\n  - name: rev\n    role: reviewer\n    depends_on: [up]\n    prompt: y\n"
	const defField = "name: p\nworkflow: wf\nsteps:\n  - name: up\n    role: writer\n    prompt: x\n  - name: rev\n    role: reviewer\n    depends_on: [up]\n    prompt: y\n"
	const want = `step "rev": role "reviewer" is not available in workflow "wf" (available: writer)`

	before := len(h.tree.SnapshotAll())
	slots := h.tree.Scheduler().Snapshot().SlotsInUse
	for _, c := range []struct{ def, arg string }{{def, "wf"}, {defField, ""}} {
		if _, err := runFn(ctx, []byte(c.def), c.arg); err == nil || err.Error() != want {
			t.Errorf("arg %q: err = %v, want %q", c.arg, err, want)
		}
	}
	if n := len(h.tree.SnapshotAll()); n != before {
		t.Fatalf("refused pipelines created %d nodes, want 0", n-before)
	}
	if got := h.tree.Scheduler().Snapshot().SlotsInUse; got != slots {
		t.Fatalf("refused pipelines left SlotsInUse %d, want %d", got, slots)
	}

	// Inside the restricted wf subtree the governing workflow limits roles.
	sub := newWiringHarness(t)
	restrictWFToWriter(sub)
	lead := sub.lead()
	subFn := makePipelineRunFn(sub.makeSpawner(lead.node, lead.node.Depth), sub.w.bb, sub.tree.Scheduler(), lead.node,
		sub.w.scope.runner(sub.makeSpawner, lead.node), sub.w.scope.governing, sub.w.scope)
	subBefore := len(sub.tree.SnapshotAll())
	if _, err := subFn(ctx, []byte(def), ""); err == nil ||
		err.Error() != `step "rev": role "reviewer" is not available in the governing workflow (available: writer)` {
		t.Fatalf("governed err = %v, want governing-workflow refusal", err)
	}
	if n := len(sub.tree.SnapshotAll()); n != subBefore {
		t.Fatalf("governed refusal created %d nodes, want 0", n-subBefore)
	}
}

// TestPipelineSynthRefusesUnofferedRoleSteps: synthesis outside any workflow
// offers every role, but a plan that sets its own workflow runs under it, so a
// step naming a role that workflow does not offer is refused before any step
// spawns.
func TestPipelineSynthRefusesUnofferedRoleSteps(t *testing.T) {
	ctx := context.Background()
	h := newWiringHarness(t)
	restrictWFToWriter(h)
	h.reply = func(opts agent.SpawnOpts) string {
		if opts.Label != "synthesize" {
			return ""
		}
		return `{"name":"p","workflow":"wf","steps":[{"name":"up","role":"writer","prompt":"x"},{"name":"rev","role":"reviewer","depends_on":["up"],"prompt":"y"}]}`
	}
	sched := h.tree.Scheduler()
	slots := sched.Snapshot().SlotsInUse
	synth := makePipelineSynthFn(h.makeSpawner(h.root, 0), h.w.bb, sched, h.root, h.cfg, nil, nil,
		h.w.scope.runner(h.makeSpawner, h.root), h.w.scope.governing)
	const want = `step "rev": role "reviewer" is not available in workflow "wf" (available: writer)`
	if _, err := synth(ctx, "goal", false, ""); err == nil || err.Error() != want {
		t.Fatalf("synth err = %v, want %q", err, want)
	}
	if n := len(nodesLabelled(h.tree, "up")) + len(nodesLabelled(h.tree, "rev")); n != 0 {
		t.Fatalf("refused synthesized pipeline spawned %d step nodes, want 0", n)
	}
	if got := sched.Snapshot().SlotsInUse; got != slots {
		t.Fatalf("refused synthesis left SlotsInUse %d, want %d", got, slots)
	}
}

// TestPipelineSynthRolePalette (change 0115): the synthesis prompt lists the
// roles a step may take: every defined role outside a workflow, the
// workflow's offered roles with one, and none at all when no roles are defined.
func TestPipelineSynthRolePalette(t *testing.T) {
	ctx := context.Background()
	h := newWiringHarness(t)
	var prompt string
	h.reply = func(opts agent.SpawnOpts) string {
		if opts.Label != "synthesize" {
			return ""
		}
		prompt = opts.SystemPrompt
		// Root synthesis requires a pool (RequirePool), so the plan names wf.
		return `{"name":"p","workflow":"wf","steps":[{"name":"s1","role":"writer","prompt":"hi"}]}`
	}
	sched := h.tree.Scheduler()
	run := h.w.scope.runner(h.makeSpawner, h.root)
	synth := makePipelineSynthFn(h.makeSpawner(h.root, 0), h.w.bb, sched, h.root, h.cfg, nil, nil, run, h.w.scope.governing)

	if line, err := synth(ctx, "goal", false, ""); err != nil || line != "pipeline p: completed" {
		t.Fatalf("root role synthesis = %q, %v; want completed", line, err)
	}
	if !strings.Contains(prompt, "- reviewer — Reviews code.\n- writer — Writes prose.\n") {
		t.Fatalf("root palette lacks both roles: %q", prompt)
	}

	wf := h.cfg.Workflows["wf"]
	wf.Roles = []string{"writer"}
	h.cfg.Workflows["wf"] = wf
	synth = makePipelineSynthFn(h.makeSpawner(h.root, 0), h.w.bb, sched, h.root, h.cfg, nil, nil, run, h.w.scope.governing)
	prompt = ""
	if _, err := synth(ctx, "goal", false, "wf"); err != nil {
		t.Fatalf("wf role synthesis: %v", err)
	}
	if !strings.Contains(prompt, "- writer — Writes prose.\n") || strings.Contains(prompt, "- reviewer") {
		t.Fatalf("wf palette = %q, want only writer", prompt)
	}

	none := h.cfg
	none.Roles = nil
	h.reply = func(opts agent.SpawnOpts) string {
		if opts.Label != "synthesize" {
			return ""
		}
		prompt = opts.SystemPrompt
		return `{"name":"p","workflow":"wf","steps":[{"name":"s1","prompt":"hi"}]}`
	}
	prompt = ""
	if _, err := makePipelineSynthFn(h.makeSpawner(h.root, 0), h.w.bb, sched, h.root, none, nil, nil, run, h.w.scope.governing)(ctx, "goal", false, ""); err != nil {
		t.Fatalf("no-roles synthesis: %v", err)
	}
	if strings.Contains(prompt, "Available roles") || strings.Contains(prompt, `"role"`) {
		t.Fatalf("no-roles synthesis prompt mentions roles: %q", prompt)
	}
}

// pipelineRunDescription returns the description of reg's pipeline_run.
func pipelineRunDescription(t *testing.T, reg *tools.Registry) string {
	t.Helper()
	for _, tl := range reg.Tools() {
		if tl.Name() == "pipeline_run" {
			return tl.Description()
		}
	}
	t.Fatal("registry has no pipeline_run")
	return ""
}

// TestWirePipelineAdvertisesRoles: pipeline_run's description lists the roles
// offered at the node, and is byte-identical to the role-free tool when no
// roles are defined.
func TestWirePipelineAdvertisesRoles(t *testing.T) {
	h := newWiringHarness(t)
	reg := tools.NewRegistry()
	h.w.wirePipeline(reg, h.root, nil)
	if d := pipelineRunDescription(t, reg); !strings.Contains(d, "\n- reviewer — Reviews code.") || !strings.Contains(d, "\n- writer — Writes prose.") {
		t.Fatalf("root pipeline_run description lacks the roles: %q", d)
	}

	plain := newWiringHarness(t)
	plain.w.scope = newWorkflowScope(plain.tree, plain.cfg.Workflows, nil)
	reg = tools.NewRegistry()
	plain.w.wirePipeline(reg, plain.root, nil)
	if got, want := pipelineRunDescription(t, reg), tools.NewPipelineRunTool(nil, nil).Description(); got != want {
		t.Fatalf("no-roles pipeline_run description changed:\n got %q\nwant %q", got, want)
	}
}
