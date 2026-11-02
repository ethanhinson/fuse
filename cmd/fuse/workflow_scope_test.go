package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/tools"
)

// scopeWF is the per-spawn workflow fixture (change 0091): a pool capped at two
// spawns and one typed reader worker with a model pin.
func scopeWF() map[string]config.WorkflowConfig {
	return map[string]config.WorkflowConfig{
		"wf": {
			Pool: config.PoolConfig{Total: 2},
			Workers: map[string]config.WorkerConfig{
				"reader": {Tools: []string{"read_file"}, Model: "pin"},
			},
		},
	}
}

// prepared is one stub-builder observation: the child node and what
// prepareChild returned for it.
type prepared struct {
	node *agent.AgentNode
	plan childPlan
	err  error
}

// scopeHarness wires a real tree and a workflowScope with stub child builders
// that call prepareChild exactly as a binding's child builder will.
type scopeHarness struct {
	t     *testing.T
	tree  *agent.AgentTree
	root  *agent.AgentNode
	scope *workflowScope

	mu   sync.Mutex
	seen []prepared
}

func newScopeHarness(t *testing.T, wfs map[string]config.WorkflowConfig) *scopeHarness {
	t.Helper()
	return newScopeHarnessRoles(t, wfs, nil)
}

// newScopeHarnessRoles is newScopeHarness with top-level roles defined.
func newScopeHarnessRoles(t *testing.T, wfs map[string]config.WorkflowConfig, roles map[string]config.RoleConfig) *scopeHarness {
	t.Helper()
	tree := agent.NewAgentTreeWithConcurrency("m", "m", 4)
	return &scopeHarness{
		t:     t,
		tree:  tree,
		root:  tree.Node(tree.RootID()),
		scope: newWorkflowScope(tree, wfs, roles),
	}
}

func (h *scopeHarness) builder(_ context.Context, opts agent.SpawnOpts, node *agent.AgentNode, _ *agent.AgentTree) (string, error) {
	plan, err := h.scope.prepareChild(opts, node)
	h.mu.Lock()
	h.seen = append(h.seen, prepared{node: node, plan: plan, err: err})
	h.mu.Unlock()
	if err != nil {
		return "", err
	}
	return "ok", nil
}

// spawner returns a Spawner at parent whose backstop is the scope's lazy one.
func (h *scopeHarness) spawner(parent *agent.AgentNode) *agent.Spawner {
	return agent.NewSpawner(
		agent.WithTree(h.tree),
		agent.WithNode(parent),
		agent.WithSpawnDepth(parent.Depth),
		agent.WithSpawnBackstop(h.scope.backstop(parent)),
		agent.WithChildBuilder(h.builder),
	)
}

// spawn runs one child from parent to completion and returns its observation.
func (h *scopeHarness) spawn(parent *agent.AgentNode, opts agent.SpawnOpts) prepared {
	h.t.Helper()
	hd, err := h.spawner(parent).Spawn(context.Background(), opts)
	if err != nil {
		h.t.Fatalf("Spawn(%+v): %v", opts, err)
	}
	hd.Wait()
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, p := range h.seen {
		if p.node.ID == hd.NodeID {
			return p
		}
	}
	h.t.Fatalf("builder never ran for node %s", hd.NodeID)
	return prepared{}
}

// lead activates wf on a root-level child and returns that workflow root.
func (h *scopeHarness) lead() *agent.AgentNode {
	h.t.Helper()
	p := h.spawn(h.root, agent.SpawnOpts{Label: "lead", Task: "t", Workflow: "wf"})
	if p.err != nil {
		h.t.Fatalf("activating lead: %v", p.err)
	}
	return p.node
}

func TestScopeNamesSortedAndCheck(t *testing.T) {
	tree := agent.NewAgentTree("root", "m")
	wfs := scopeWF()
	wfs["alpha"] = config.WorkflowConfig{}
	wfs["research"] = config.WorkflowConfig{}
	s := newWorkflowScope(tree, wfs, nil)
	if got, want := s.names(), []string{"alpha", "research", "wf"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("names() = %v, want %v", got, want)
	}
	if err := s.check("wf"); err != nil {
		t.Fatalf("check(wf) = %v, want nil", err)
	}

	one := newWorkflowScope(tree, scopeWF(), nil)
	err := one.check("nope")
	if err == nil || err.Error() != `unknown workflow "nope" (available: wf)` {
		t.Fatalf("check(nope) = %v, want the unknown-workflow listing", err)
	}

	none := newWorkflowScope(tree, nil, nil)
	err = none.check("nope")
	if err == nil || err.Error() != `unknown workflow "nope" (available: none configured)` {
		t.Fatalf("check with no workflows = %v, want (none configured)", err)
	}
	if len(none.names()) != 0 {
		t.Fatalf("names() with no workflows = %v, want empty", none.names())
	}
}

func TestPrepareChildActivatesWorkflow(t *testing.T) {
	h := newScopeHarness(t, scopeWF())
	p := h.spawn(h.root, agent.SpawnOpts{Label: "lead", Task: "t", Workflow: "wf"})
	if p.err != nil {
		t.Fatalf("prepareChild: %v", p.err)
	}
	pool, ok := h.tree.Scheduler().Pool(p.node.ID)
	if !ok || pool.Total != 2 {
		t.Fatalf("Pool(lead) = %+v, %v; want Total 2 registered", pool, ok)
	}
	act, rootID := h.scope.activationOf(p.node.ID)
	if act == nil {
		t.Fatal("activationOf(lead) = nil, want the wf activation")
	}
	if rootID != p.node.ID || act.name != "wf" {
		t.Fatalf("activationOf(lead) = (%q, %q), want (wf, %s)", act.name, rootID, p.node.ID)
	}
	if act.rootDepth != p.node.Depth {
		t.Fatalf("rootDepth = %d, want lead depth %d", act.rootDepth, p.node.Depth)
	}
	// Idempotent: a second lookup returns the same activation (same counter).
	if again, _ := h.scope.activationOf(p.node.ID); again != act {
		t.Fatal("activationOf is not stable across calls")
	}
	// The plain root is outside every workflow.
	if a, id := h.scope.activationOf(h.root.ID); a != nil || id != "" {
		t.Fatalf("activationOf(root) = (%v, %q), want (nil, \"\")", a, id)
	}
}

func TestPrepareChildKeysActivationsByRoot(t *testing.T) {
	// Two roots of the same workflow each get their own activation (and quota).
	h := newScopeHarness(t, scopeWF())
	a := h.lead()
	b := h.lead()
	actA, _ := h.scope.activationOf(a.ID)
	actB, _ := h.scope.activationOf(b.ID)
	if actA == nil || actB == nil || actA == actB {
		t.Fatalf("activations for two roots = %p, %p; want two distinct", actA, actB)
	}
}

func TestPrepareChildResolvesWorkerInSubtree(t *testing.T) {
	h := newScopeHarness(t, scopeWF())
	lead := h.lead()

	p := h.spawn(lead, agent.SpawnOpts{Label: "r", Task: "t", Worker: "reader"})
	if p.err != nil {
		t.Fatalf("prepareChild(reader): %v", p.err)
	}
	if !reflect.DeepEqual(p.plan.tools, []string{"read_file"}) || !p.plan.bound || p.plan.model != "pin" {
		t.Fatalf("plan = %+v, want tools [read_file], bound, model pin", p.plan)
	}

	g := h.spawn(lead, agent.SpawnOpts{Label: "g", Task: "t", Worker: "ghost"})
	if g.err == nil || !strings.Contains(g.err.Error(), `unknown worker "ghost"`) {
		t.Fatalf("prepareChild(ghost) err = %v, want unknown worker", g.err)
	}

	// A freeform spawn inside the workflow is not bound and keeps its subset. It
	// uses a second root: the ghost spawn above was admitted (and spent a pool
	// slot) before prepareChild refused it.
	f := h.spawn(h.lead(), agent.SpawnOpts{Label: "f", Task: "t", Tools: []string{"bash"}})
	if f.err != nil || f.plan.bound || !reflect.DeepEqual(f.plan.tools, []string{"bash"}) || f.plan.model != "" {
		t.Fatalf("freeform plan = %+v, %v; want tools [bash], unbound", f.plan, f.err)
	}
}

func TestPrepareChildRefusesWorkerOutsideWorkflow(t *testing.T) {
	// Spawner.Spawn directly (the pipeline-step path): a worker named where no
	// workflow is active is refused, not ignored.
	h := newScopeHarness(t, scopeWF())
	p := h.spawn(h.root, agent.SpawnOpts{Label: "x", Task: "t", Worker: "reader", Tools: []string{"bash"}})
	if !errors.Is(p.err, agent.ErrWorkerOutsideWorkflow) {
		t.Fatalf("prepareChild err = %v, want ErrWorkerOutsideWorkflow", p.err)
	}
	if !strings.Contains(p.err.Error(), "pass workflow") {
		t.Fatalf("prepareChild err = %q, want it to say pass workflow", p.err)
	}
}

func TestCheckRequest_WorkerOutsideWorkflowRefused(t *testing.T) {
	h := newScopeHarness(t, scopeWF())

	err := h.scope.checkRequest(h.root, tools.SpawnRequest{Worker: "reader"})
	if !errors.Is(err, agent.ErrWorkerOutsideWorkflow) {
		t.Fatalf("checkRequest(root, \"\", reader) = %v, want ErrWorkerOutsideWorkflow", err)
	}
	if want := `worker "reader" named outside any workflow; pass workflow (available: wf)`; !strings.Contains(err.Error(), want) {
		t.Fatalf("checkRequest err = %q, want it to contain %q", err, want)
	}

	if err := h.scope.checkRequest(h.root, tools.SpawnRequest{Workflow: "wf", Worker: "reader"}); err != nil {
		t.Fatalf("checkRequest(root, wf, reader) = %v, want nil", err)
	}

	lead := h.lead()
	if err := h.scope.checkRequest(lead, tools.SpawnRequest{Worker: "reader"}); err != nil {
		t.Fatalf("checkRequest(lead, \"\", reader) = %v, want nil inside the subtree", err)
	}

	none := newScopeHarness(t, nil)
	err = none.scope.checkRequest(none.root, tools.SpawnRequest{Worker: "reader"})
	if !errors.Is(err, agent.ErrWorkerOutsideWorkflow) || !strings.Contains(err.Error(), "(available: none configured)") {
		t.Fatalf("checkRequest with no workflows = %v, want ErrWorkerOutsideWorkflow listing none configured", err)
	}

	// Through the spawn func: the refusal is a tool error that creates no node.
	before := len(h.tree.SnapshotAll())
	_, err = h.scope.spawnFunc(h.spawner(h.root), h.root)(context.Background(), tools.SpawnRequest{Label: "x", Task: "t", Worker: "reader"})
	if !errors.Is(err, agent.ErrWorkerOutsideWorkflow) || !strings.Contains(err.Error(), "pass workflow (available: wf)") {
		t.Fatalf("spawnFunc(Worker reader at root) = %v, want ErrWorkerOutsideWorkflow", err)
	}
	if after := len(h.tree.SnapshotAll()); after != before {
		t.Fatalf("node count %d -> %d, want the refusal to create no node", before, after)
	}
}

func TestScopeBackstopEnforcesTotal(t *testing.T) {
	h := newScopeHarness(t, scopeWF())
	lead := h.lead()
	newDepth := lead.Depth + 1

	bs := h.scope.backstop(lead)
	for i := 1; i <= 2; i++ {
		if err := bs(newDepth); err != nil {
			t.Fatalf("backstop call %d = %v, want admitted", i, err)
		}
	}
	if err := bs(newDepth); !errors.Is(err, agent.ErrWorkflowQuotaExhausted) {
		t.Fatalf("backstop call 3 = %v, want ErrWorkflowQuotaExhausted", err)
	}
	// A second backstop for the same root shares the activation's counter.
	if err := h.scope.backstop(lead)(newDepth); !errors.Is(err, agent.ErrWorkflowQuotaExhausted) {
		t.Fatalf("backstop call 4 (fresh backstop) = %v, want still refused", err)
	}
	// Outside any workflow the backstop never refuses.
	out := h.scope.backstop(h.root)
	for i := 0; i < 5; i++ {
		if err := out(1); err != nil {
			t.Fatalf("root backstop = %v, want nil outside a workflow", err)
		}
	}
}

func TestScopeBackstopEnforcesTotalThroughSpawner(t *testing.T) {
	// End to end through the real Spawner: the lead (a workflow root) is admitted
	// into its PARENT's pool, so it does not count; its two children do, and the
	// third is refused before any node exists.
	h := newScopeHarness(t, scopeWF())
	lead := h.lead()
	sp := h.spawner(lead)
	for i := 0; i < 2; i++ {
		hd, err := sp.Spawn(context.Background(), agent.SpawnOpts{Label: "c", Task: "t"})
		if err != nil {
			t.Fatalf("spawn %d: %v", i+1, err)
		}
		hd.Wait()
	}
	before := len(h.tree.SnapshotAll())
	if _, err := sp.Spawn(context.Background(), agent.SpawnOpts{Label: "c", Task: "t"}); !errors.Is(err, agent.ErrWorkflowQuotaExhausted) {
		t.Fatalf("third spawn = %v, want ErrWorkflowQuotaExhausted", err)
	}
	if after := len(h.tree.SnapshotAll()); after != before {
		t.Fatalf("node count %d -> %d, want no node for the refused spawn", before, after)
	}
}

func TestScopeSpawnFuncRefusesBeforeSpawn(t *testing.T) {
	h := newScopeHarness(t, scopeWF())
	lead := h.lead()
	before := len(h.tree.SnapshotAll())

	rootFn := h.scope.spawnFunc(h.spawner(h.root), h.root)
	if _, err := rootFn(context.Background(), tools.SpawnRequest{Label: "x", Task: "t", Workflow: "nope"}); err == nil || !strings.Contains(err.Error(), `unknown workflow "nope"`) {
		t.Fatalf("Workflow nope = %v, want unknown workflow", err)
	}
	if _, err := rootFn(context.Background(), tools.SpawnRequest{Label: "x", Task: "t", Workflow: "wf", Worker: "ghost"}); err == nil || !strings.Contains(err.Error(), `unknown worker "ghost"`) {
		t.Fatalf("Workflow wf + Worker ghost = %v, want unknown worker", err)
	}

	leadFn := h.scope.spawnFunc(h.spawner(lead), lead)
	if _, err := leadFn(context.Background(), tools.SpawnRequest{Label: "x", Task: "t", Worker: "ghost"}); err == nil || !strings.Contains(err.Error(), `unknown worker "ghost"`) {
		t.Fatalf("Worker ghost inside lead = %v, want unknown worker", err)
	}
	if after := len(h.tree.SnapshotAll()); after != before {
		t.Fatalf("node count %d -> %d, want refusals to create no node", before, after)
	}
	// The refused requests spent no quota: both pool slots are still available.
	act, _ := h.scope.activationOf(lead.ID)
	if n := act.reserved.Load(); n != 0 {
		t.Fatalf("reserved = %d after refusals, want 0", n)
	}

	// A valid request goes through and forwards Workflow to the Spawner.
	hd, err := rootFn(context.Background(), tools.SpawnRequest{Label: "lead2", Task: "t", Workflow: "wf", Worker: "reader"})
	if err != nil {
		t.Fatalf("valid workflow spawn: %v", err)
	}
	if res := hd.WaitResult(); res.Err != nil {
		t.Fatalf("valid workflow spawn result: %v", res.Err)
	}
	var lead2 *agent.AgentNode
	for _, v := range h.tree.SnapshotAll() {
		if v.Label == "lead2" {
			lead2 = h.tree.Node(v.ID)
		}
	}
	if lead2 == nil || lead2.Snapshot().WorkflowRoot != "wf" {
		t.Fatal("spawnFunc did not create lead2 as a wf root")
	}

	// A nested request inside the lead is left to the Spawner's refusal.
	if _, err := leadFn(context.Background(), tools.SpawnRequest{Label: "n", Task: "t", Workflow: "wf"}); !errors.Is(err, agent.ErrNestedWorkflow) {
		t.Fatalf("nested workflow = %v, want ErrNestedWorkflow", err)
	}
}

func spawnToolProps(t *testing.T, st *tools.SpawnAgentTool) map[string]any {
	t.Helper()
	props, ok := st.Parameters()["properties"].(map[string]any)
	if !ok {
		t.Fatal("spawn_agent parameters have no properties map")
	}
	return props
}

func TestScopeSpawnToolAdvertisesByPosition(t *testing.T) {
	h := newScopeHarness(t, scopeWF())
	lead := h.lead()
	fn := func(context.Context, tools.SpawnRequest) (tools.SpawnHandle, error) { return nil, nil }

	rootProps := spawnToolProps(t, h.scope.spawnTool(fn, h.root))
	if _, ok := rootProps["workflow"]; !ok {
		t.Fatal("root spawn_agent lacks a workflow property")
	}
	if _, ok := rootProps["worker"]; ok {
		t.Fatal("root spawn_agent advertises worker outside a workflow")
	}

	leadProps := spawnToolProps(t, h.scope.spawnTool(fn, lead))
	if _, ok := leadProps["workflow"]; ok {
		t.Fatal("lead spawn_agent advertises workflow inside a workflow")
	}
	w, ok := leadProps["worker"].(map[string]any)
	if !ok {
		t.Fatal("lead spawn_agent lacks a worker property")
	}
	if got := w["enum"]; !reflect.DeepEqual(got, []any{"reader"}) {
		t.Fatalf("worker enum = %#v, want [reader]", got)
	}
}

func TestActivateRootMarksRoot(t *testing.T) {
	tree := agent.NewAgentTreeWithConcurrency("m", "m", 4)
	root := tree.Node(tree.RootID())
	s := newWorkflowScope(tree, scopeWF(), nil)

	if _, err := s.activateRoot(root, "nope"); err == nil || !strings.Contains(err.Error(), `unknown workflow "nope"`) {
		t.Fatalf("activateRoot(nope) = %v, want unknown workflow", err)
	}
	if root.Snapshot().WorkflowRoot != "" {
		t.Fatal("a refused activateRoot marked the root")
	}

	act, err := s.activateRoot(root, "wf")
	if err != nil {
		t.Fatalf("activateRoot(wf): %v", err)
	}
	if root.Snapshot().WorkflowRoot != "wf" {
		t.Fatalf("root WorkflowRoot = %q, want wf", root.Snapshot().WorkflowRoot)
	}
	if pool, ok := tree.Scheduler().Pool(root.ID); !ok || pool.Total != 2 {
		t.Fatalf("Pool(root) = %+v, %v; want Total 2", pool, ok)
	}
	if got, id := s.activationOf(root.ID); got != act || id != root.ID {
		t.Fatalf("activationOf(root) = (%p, %q), want the activateRoot result", got, id)
	}
}

func TestScopeActivationOfLazilyActivatesUnpreparedRoot(t *testing.T) {
	// A workflow root created on a path that skipped prepareChild is activated on
	// first lookup, so its pool and quota still hold.
	h := newScopeHarness(t, scopeWF())
	sp := agent.NewSpawner(
		agent.WithTree(h.tree), agent.WithNode(h.root), agent.WithSpawnDepth(0),
		agent.WithChildBuilder(func(context.Context, agent.SpawnOpts, *agent.AgentNode, *agent.AgentTree) (string, error) {
			return "ok", nil
		}),
	)
	hd, err := sp.Spawn(context.Background(), agent.SpawnOpts{Label: "raw", Task: "t", Workflow: "wf"})
	if err != nil {
		t.Fatal(err)
	}
	hd.Wait()
	act, rootID := h.scope.activationOf(hd.NodeID)
	if act == nil || rootID != hd.NodeID {
		t.Fatalf("activationOf(raw) = (%v, %q), want a lazy activation", act, rootID)
	}
	if pool, ok := h.tree.Scheduler().Pool(hd.NodeID); !ok || pool.Total != 2 {
		t.Fatalf("Pool(raw) = %+v, %v; want Total 2", pool, ok)
	}
}

func TestScopeConcurrentActivationIsSingle(t *testing.T) {
	// Concurrent lookups of one unactivated root agree on one activation, so the
	// quota counter is never split.
	h := newScopeHarness(t, scopeWF())
	sp := agent.NewSpawner(
		agent.WithTree(h.tree), agent.WithNode(h.root), agent.WithSpawnDepth(0),
		agent.WithChildBuilder(func(context.Context, agent.SpawnOpts, *agent.AgentNode, *agent.AgentTree) (string, error) {
			return "ok", nil
		}),
	)
	hd, err := sp.Spawn(context.Background(), agent.SpawnOpts{Label: "raw", Task: "t", Workflow: "wf"})
	if err != nil {
		t.Fatal(err)
	}
	hd.Wait()
	const n = 16
	got := make([]*workflowActivation, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got[i], _ = h.scope.activationOf(hd.NodeID)
		}(i)
	}
	wg.Wait()
	for i := 1; i < n; i++ {
		if got[i] == nil || got[i] != got[0] {
			t.Fatalf("concurrent activationOf returned distinct activations")
		}
	}
}

func TestScopeActivateWorkflowIsIdempotent(t *testing.T) {
	// Re-activating a root (a second prepareChild, a lazy lookup, activateRoot
	// after a lookup) must return the same activation: a fresh one would reset
	// the reserved counter and re-open a spent pool.total.
	h := newScopeHarness(t, scopeWF())
	lead := h.lead()
	first, _ := h.scope.activationOf(lead.ID)
	bs := h.scope.backstop(lead)
	for i := 0; i < 2; i++ {
		if err := bs(lead.Depth + 1); err != nil {
			t.Fatalf("reserve %d: %v", i+1, err)
		}
	}
	again, err := h.scope.activateWorkflow(lead)
	if err != nil {
		t.Fatalf("re-activate: %v", err)
	}
	if again != first {
		t.Fatal("activateWorkflow on an active root returned a new activation")
	}
	if p := h.spawn(h.root, agent.SpawnOpts{Label: "lead", Task: "t", Workflow: "wf"}); p.err != nil {
		t.Fatalf("unrelated root: %v", p.err)
	}
	if err := bs(lead.Depth + 1); !errors.Is(err, agent.ErrWorkflowQuotaExhausted) {
		t.Fatalf("after re-activation backstop = %v, want the spent quota still refused", err)
	}
}

// scopeRoles is the role fixture (change 0114): reviewer has its own tool set
// and a model pin; writer inherits the parent's tools.
func scopeRoles() map[string]config.RoleConfig {
	return map[string]config.RoleConfig{
		"reviewer": {Description: "Reviews code.", Instructions: "REVIEWER-BRIEF", Tools: []string{"read_file", "grep"}, Model: "rmodel"},
		"writer":   {Description: "Writes prose."},
	}
}

// scopeWFWithWriter is scopeWF whose wf offers only the writer role.
func scopeWFWithWriter() map[string]config.WorkflowConfig {
	wfs := scopeWF()
	wf := wfs["wf"]
	wf.Roles = []string{"writer"}
	wfs["wf"] = wf
	return wfs
}

func TestCheckRequestRefusesRoleMisuse(t *testing.T) {
	roles := scopeRoles()
	w := roles["writer"]
	w.Instructions = "WRITER-BRIEF"
	roles["writer"] = w
	h := newScopeHarnessRoles(t, scopeWFWithWriter(), roles)
	lead := h.lead()

	cases := []struct {
		name   string
		parent *agent.AgentNode
		req    tools.SpawnRequest
		want   []string
	}{
		{"role+system_prompt", h.root, tools.SpawnRequest{Role: "reviewer", SystemPrompt: "x"},
			[]string{"a role supplies the system prompt; drop system_prompt or the role"}},
		{"role+worker", lead, tools.SpawnRequest{Role: "writer", Worker: "reader"},
			[]string{`role "writer" and worker "reader" cannot be combined; pass one`}},
		{"unknown", h.root, tools.SpawnRequest{Role: "ghost"},
			[]string{`unknown role "ghost" (available: reviewer, writer)`}},
		{"widening", h.root, tools.SpawnRequest{Role: "reviewer", Tools: []string{"read_file", "bash"}},
			[]string{`role "reviewer" does not permit tool "bash"`}},
		{"not offered", lead, tools.SpawnRequest{Role: "reviewer"},
			[]string{`role "reviewer" is not available in workflow "wf" (available: writer)`}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := len(h.tree.SnapshotAll())
			err := h.scope.checkRequest(c.parent, c.req)
			if err == nil {
				t.Fatalf("checkRequest(%+v) = nil, want refusal", c.req)
			}
			for _, w := range c.want {
				if !strings.Contains(err.Error(), w) {
					t.Fatalf("checkRequest err = %q, want it to contain %q", err, w)
				}
			}
			// Through the spawn func as well: a tool error, no node.
			c.req.Label, c.req.Task = "x", "t"
			if _, err := h.scope.spawnFunc(h.spawner(c.parent), c.parent)(context.Background(), c.req); err == nil {
				t.Fatalf("spawnFunc(%+v) = nil error, want refusal", c.req)
			}
			if after := len(h.tree.SnapshotAll()); after != before {
				t.Fatalf("node count %d -> %d, want the refusal to create no node", before, after)
			}
		})
	}

	for _, ok := range []struct {
		parent *agent.AgentNode
		req    tools.SpawnRequest
	}{
		{h.root, tools.SpawnRequest{Role: "reviewer"}},
		{h.root, tools.SpawnRequest{Role: "reviewer", Tools: []string{"grep"}}},
		{lead, tools.SpawnRequest{Role: "writer"}},
	} {
		if err := h.scope.checkRequest(ok.parent, ok.req); err != nil {
			t.Fatalf("checkRequest(%+v) = %v, want nil", ok.req, err)
		}
	}
}

func TestCheckRequestUnknownRoleWithNoRolesConfigured(t *testing.T) {
	h := newScopeHarness(t, scopeWF())
	err := h.scope.checkRequest(h.root, tools.SpawnRequest{Role: "x"})
	if err == nil || !strings.Contains(err.Error(), `unknown role "x" (available: none configured)`) {
		t.Fatalf("checkRequest(role x, no roles) = %v, want none configured", err)
	}
}

func TestPrepareChildRolePlan(t *testing.T) {
	roles := scopeRoles()
	w := roles["writer"]
	w.Instructions = "WRITER-BRIEF"
	roles["writer"] = w
	h := newScopeHarnessRoles(t, scopeWF(), roles)

	p := h.spawn(h.root, agent.SpawnOpts{Label: "r", Task: "t", Role: "reviewer"})
	want := childPlan{tools: []string{"read_file", "grep"}, bound: true, model: "rmodel", instructions: "REVIEWER-BRIEF"}
	if p.err != nil || !reflect.DeepEqual(p.plan, want) {
		t.Fatalf("reviewer plan = %+v, %v; want %+v", p.plan, p.err, want)
	}

	p = h.spawn(h.root, agent.SpawnOpts{Label: "r2", Task: "t", Role: "reviewer", Tools: []string{"grep"}})
	if p.err != nil || !reflect.DeepEqual(p.plan.tools, []string{"grep"}) || !p.plan.bound {
		t.Fatalf("reviewer narrowed plan = %+v, %v; want tools [grep], bound", p.plan, p.err)
	}

	p = h.spawn(h.root, agent.SpawnOpts{Label: "w", Task: "t", Role: "writer"})
	want = childPlan{instructions: "WRITER-BRIEF"}
	if p.err != nil || !reflect.DeepEqual(p.plan, want) {
		t.Fatalf("writer plan = %+v, %v; want %+v", p.plan, p.err, want)
	}

	p = h.spawn(h.root, agent.SpawnOpts{Label: "w2", Task: "t", Role: "writer", Tools: []string{"read_file"}})
	if p.err != nil || !reflect.DeepEqual(p.plan.tools, []string{"read_file"}) || p.plan.bound {
		t.Fatalf("writer narrowed plan = %+v, %v; want tools [read_file], unbound", p.plan, p.err)
	}

	// Defense in depth for paths that call Spawner.Spawn directly.
	p = h.spawn(h.root, agent.SpawnOpts{Label: "x", Task: "t", Role: "reviewer", SystemPrompt: "x"})
	if p.err == nil || !strings.Contains(p.err.Error(), "a role supplies the system prompt") {
		t.Fatalf("prepareChild(role+system_prompt) err = %v, want refusal", p.err)
	}
}

func TestSpawnToolAdvertisesOfferedRoles(t *testing.T) {
	fn := func(context.Context, tools.SpawnRequest) (tools.SpawnHandle, error) { return nil, nil }
	h := newScopeHarnessRoles(t, scopeWFWithWriter(), scopeRoles())
	lead := h.lead()

	enumOf := func(node *agent.AgentNode) any {
		t.Helper()
		r, ok := spawnToolProps(t, h.scope.spawnTool(fn, node))["role"].(map[string]any)
		if !ok {
			t.Fatalf("spawn_agent at %s lacks a role property", node.ID)
		}
		return r["enum"]
	}
	if got := enumOf(h.root); !reflect.DeepEqual(got, []any{"reviewer", "writer"}) {
		t.Fatalf("root role enum = %#v, want [reviewer writer]", got)
	}
	if got := enumOf(lead); !reflect.DeepEqual(got, []any{"writer"}) {
		t.Fatalf("lead role enum = %#v, want [writer]", got)
	}

	none := newScopeHarness(t, scopeWF())
	if _, ok := spawnToolProps(t, none.scope.spawnTool(fn, none.root))["role"]; ok {
		t.Fatal("spawn_agent advertises role with no roles defined")
	}
}

// A spawn naming both a role and a workflow creates that workflow's lead, which
// sits inside the workflow's subtree: the role is checked against the named
// workflow's offered roles, not the (workflow-less) parent's.
func TestRoleWithWorkflowCheckedAgainstThatWorkflow(t *testing.T) {
	wfs := scopeWFWithWriter()
	wfs["open"] = config.WorkflowConfig{Pool: config.PoolConfig{Total: 2}}
	h := newScopeHarnessRoles(t, wfs, scopeRoles())

	// Not listed by wf: refused up front, with no node and no activation.
	before := len(h.tree.SnapshotAll())
	err := h.scope.checkRequest(h.root, tools.SpawnRequest{Role: "reviewer", Workflow: "wf"})
	want := `role "reviewer" is not available in workflow "wf" (available: writer)`
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("checkRequest(reviewer, wf) = %v, want %q", err, want)
	}
	if _, err := h.scope.spawnFunc(h.spawner(h.root), h.root)(context.Background(),
		tools.SpawnRequest{Label: "x", Task: "t", Role: "reviewer", Workflow: "wf"}); err == nil {
		t.Fatal("spawnFunc(reviewer, wf) = nil error, want refusal")
	}
	if after := len(h.tree.SnapshotAll()); after != before {
		t.Fatalf("node count %d -> %d, want the refusal to create no node", before, after)
	}

	// Defense in depth: the direct Spawner.Spawn path refuses too, before activating.
	p := h.spawn(h.root, agent.SpawnOpts{Label: "x", Task: "t", Role: "reviewer", Workflow: "wf"})
	if p.err == nil || !strings.Contains(p.err.Error(), want) {
		t.Fatalf("prepareChild(reviewer, wf) err = %v, want %q", p.err, want)
	}
	h.scope.mu.Lock()
	_, activated := h.scope.active[p.node.ID]
	h.scope.mu.Unlock()
	if activated {
		t.Fatal("refused role+workflow child activated the workflow")
	}

	// Listed by wf: accepted.
	if err := h.scope.checkRequest(h.root, tools.SpawnRequest{Role: "writer", Workflow: "wf"}); err != nil {
		t.Fatalf("checkRequest(writer, wf) = %v, want nil", err)
	}
	if p := h.spawn(h.root, agent.SpawnOpts{Label: "w", Task: "t", Role: "writer", Workflow: "wf"}); p.err != nil || p.node.Role != "writer" {
		t.Fatalf("prepareChild(writer, wf) = %+v, %v; want writer plan", p.plan, p.err)
	}

	// A workflow listing no roles offers every defined role.
	for _, r := range []string{"reviewer", "writer"} {
		if err := h.scope.checkRequest(h.root, tools.SpawnRequest{Role: r, Workflow: "open"}); err != nil {
			t.Fatalf("checkRequest(%s, open) = %v, want nil", r, err)
		}
		if p := h.spawn(h.root, agent.SpawnOpts{Label: r, Task: "t", Role: r, Workflow: "open"}); p.err != nil || p.node.Role != r {
			t.Fatalf("prepareChild(%s, open) = %+v, %v; want %s plan", r, p.plan, p.err, r)
		}
	}
}
