package main

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

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

// newToolLessWiringHarness is newWiringHarness plus a tool-less judge role. It lives
// here, not in the shared fixtures, so the role enums other tests pin stay as they are.
func newToolLessWiringHarness(t *testing.T) *wiringHarness {
	t.Helper()
	return newWiringHarnessCfg(t, 4, func(cfg *config.Config) {
		cfg.Roles["judge"] = config.RoleConfig{Description: "Judges.", NoTools: true}
	})
}

// newWiringHarnessCfg builds the wiring harness, letting edit adjust the
// fixture config before the scope is built from it. A nil edit is a no-op.
func newWiringHarnessCfg(t *testing.T, slots int, edit func(*config.Config)) *wiringHarness {
	t.Helper()
	cfg := config.Default()
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
	h.w = loopWiring{
		cfg:         cfg,
		scope:       newRoleScope(tree, cfg.Roles),
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

// makeSpawner mirrors a binding's makeSpawner over the shared child builder.
func (h *wiringHarness) makeSpawner(parent *agent.AgentNode, depth int) *agent.Spawner {
	return agent.NewSpawner(
		agent.WithTree(h.tree),
		agent.WithNode(parent),
		agent.WithSpawnDepth(depth),
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

// TestRoleChildToolsReviewerBound: a role with a non-empty tool set is the
// child's whole tool set, with no spawn_agent, and its plan carries the brief and model pin
// every child builder applies (change 0114).
func TestRoleChildToolsReviewerBound(t *testing.T) {
	h := newWiringHarness(t)
	rev := h.spawn(h.root, agent.SpawnOpts{Label: "rev", Task: "t", Role: "reviewer"})

	want := []string{"grep", "read_file"}
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
	if !slices.Contains(names, "spawn_agent") {
		t.Fatalf("freeform schema %v lacks spawn_agent", names)
	}
	if free.model != "" {
		t.Fatalf("freeform model pin = %q, want none", free.model)
	}

	narrow := h.spawn(h.root, agent.SpawnOpts{Label: "narrow", Task: "t", Tools: []string{"read_file"}})
	if got := schemaNames(narrow.reg); !reflect.DeepEqual(got, []string{"read_file"}) {
		t.Fatalf("subset schema = %v, want [read_file]", got)
	}
}

func TestRootToolsRegistersScopedTools(t *testing.T) {
	h := newWiringHarness(t)
	reg := defaultToolRegistry(nil, h.cfg.Research, nil)
	h.w.rootTools(reg, h.root)

	names := schemaNames(reg)
	if !slices.Contains(names, "spawn_agent") {
		t.Fatalf("root schema %v lacks spawn_agent", names)
	}
}

// TestChildToolsToolLessRole: a tool-less role child gets an empty registry:
// no tools and no spawn_agent, and its plan is marked none so every child builder composes the tool-less prompt.
func TestChildToolsToolLessRole(t *testing.T) {
	h := newToolLessWiringHarness(t)
	j := h.spawn(h.root, agent.SpawnOpts{Label: "j", Task: "t", Role: "judge"})

	if n := len(j.reg.Tools()); n != 0 {
		t.Fatalf("tool-less role registry has %d tools %v, want none", n, schemaNames(j.reg))
	}
	if !j.plan.none {
		t.Fatalf("tool-less role plan = %+v, want none", j.plan)
	}
	if j.plan.tools != nil {
		t.Fatalf("tool-less role plan = %+v, want no tool list", j.plan)
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
	if free.plan.none || !slices.Contains(schemaNames(free.reg), "spawn_agent") {
		t.Fatalf("freeform plan = %+v with %v, want an ordinary child with spawn_agent", free.plan, schemaNames(free.reg))
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
	for _, o := range h.w.scope.roleOptions() {
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

const wiringPipelineDef = "name: p\nsteps:\n  - name: s1\n    worker: reader\n    prompt: hi\n"

// wiringPlainPipelineDef names no worker: outside a workflow a worker step is
// refused (agent.ErrWorkerOutsideWorkflow), so the plain path runs freeform steps.
const wiringPlainPipelineDef = "name: p\nsteps:\n  - name: s1\n    prompt: hi\n"

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

func scopeRoles() map[string]config.RoleConfig {
	return map[string]config.RoleConfig{
		"reviewer": {Description: "Reviews code.", Instructions: "REVIEWER-BRIEF", Tools: []string{"read_file", "grep"}, Model: "rmodel"},
		"writer":   {Description: "Writes prose."},
	}
}

// TestResolveRoleTools_ToolLessRole: a tool-less role resolves to no tool list and any
// requested tool is refused.
func TestResolveRoleTools_ToolLessRole(t *testing.T) {
	role := config.RoleConfig{NoTools: true}
	got, err := resolveRoleTools("judge", role, nil)
	if err != nil || got != nil {
		t.Fatalf("resolveRoleTools(judge, nil) = (%v, %v), want (nil, nil)", got, err)
	}
	got, err = resolveRoleTools("judge", role, []string{"read_file"})
	if got != nil {
		t.Errorf("requested [read_file]: got %v, want nil", got)
	}
	if err == nil || !strings.Contains(err.Error(), `role "judge" permits no tools`) {
		t.Fatalf("requested [read_file]: err = %v, want permits no tools", err)
	}
}

func TestCheckRequestRefusesRoleMisuse(t *testing.T) {
	h := newWiringHarness(t)
	cases := []struct {
		name string
		req  tools.SpawnRequest
		want string
	}{
		{"role+system_prompt", tools.SpawnRequest{Role: "reviewer", SystemPrompt: "x"},
			"a role supplies the system prompt; drop system_prompt or the role"},
		{"unknown", tools.SpawnRequest{Role: "ghost"}, `unknown role "ghost" (available: reviewer, writer)`},
		{"widening", tools.SpawnRequest{Role: "reviewer", Tools: []string{"read_file", "bash"}},
			`role "reviewer" does not permit tool "bash"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := len(h.tree.SnapshotAll())
			err := h.w.scope.checkRequest(c.req)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("checkRequest(%+v) = %v, want it to contain %q", c.req, err, c.want)
			}
			// Through the spawn func as well: a tool error, no node.
			c.req.Label, c.req.Task = "x", "t"
			sp := h.makeSpawner(h.root, h.root.Depth)
			if _, err := h.w.scope.spawnFunc(sp, h.root)(context.Background(), c.req); err == nil {
				t.Fatalf("spawnFunc(%+v) = nil error, want refusal", c.req)
			}
			if after := len(h.tree.SnapshotAll()); after != before {
				t.Fatalf("node count %d -> %d, want the refusal to create no node", before, after)
			}
		})
	}
	for _, req := range []tools.SpawnRequest{
		{Role: "reviewer"},
		{Role: "reviewer", Tools: []string{"grep"}},
		{Role: "writer"},
	} {
		if err := h.w.scope.checkRequest(req); err != nil {
			t.Fatalf("checkRequest(%+v) = %v, want nil", req, err)
		}
	}
}

func TestCheckRequestUnknownRoleWithNoRolesConfigured(t *testing.T) {
	scope := newRoleScope(agent.NewAgentTree("root", "m"), nil)
	err := scope.checkRequest(tools.SpawnRequest{Role: "x"})
	if err == nil || !strings.Contains(err.Error(), `unknown role "x" (available: none configured)`) {
		t.Fatalf("checkRequest(role x, no roles) = %v, want none configured", err)
	}
}

func TestPrepareChildRolePlan(t *testing.T) {
	roles := scopeRoles()
	w := roles["writer"]
	w.Instructions = "WRITER-BRIEF"
	roles["writer"] = w
	scope := newRoleScope(agent.NewAgentTree("root", "m"), roles)

	p, err := scope.prepareChild(agent.SpawnOpts{Label: "r", Task: "t", Role: "reviewer"})
	want := childPlan{tools: []string{"read_file", "grep"}, model: "rmodel", instructions: roles["reviewer"].Instructions}
	if err != nil || !reflect.DeepEqual(p, want) {
		t.Fatalf("reviewer plan = %+v, %v; want %+v", p, err, want)
	}

	p, err = scope.prepareChild(agent.SpawnOpts{Label: "r2", Task: "t", Role: "reviewer", Tools: []string{"grep"}})
	if err != nil || !reflect.DeepEqual(p.tools, []string{"grep"}) {
		t.Fatalf("reviewer narrowed plan = %+v, %v; want tools [grep]", p, err)
	}

	p, err = scope.prepareChild(agent.SpawnOpts{Label: "w", Task: "t", Role: "writer"})
	want = childPlan{instructions: "WRITER-BRIEF"}
	if err != nil || !reflect.DeepEqual(p, want) {
		t.Fatalf("writer plan = %+v, %v; want %+v", p, err, want)
	}

	p, err = scope.prepareChild(agent.SpawnOpts{Label: "w2", Task: "t", Role: "writer", Tools: []string{"read_file"}})
	if err != nil || !reflect.DeepEqual(p.tools, []string{"read_file"}) {
		t.Fatalf("writer narrowed plan = %+v, %v; want tools [read_file]", p, err)
	}

	// Defense in depth for paths that call Spawner.Spawn directly.
	_, err = scope.prepareChild(agent.SpawnOpts{Label: "x", Task: "t", Role: "reviewer", SystemPrompt: "x"})
	if err == nil || !strings.Contains(err.Error(), "a role supplies the system prompt") {
		t.Fatalf("prepareChild(role+system_prompt) err = %v, want refusal", err)
	}
}

func TestSpawnToolAdvertisesRoles(t *testing.T) {
	fn := func(context.Context, tools.SpawnRequest) (tools.SpawnHandle, error) { return nil, nil }
	props := func(st *tools.SpawnAgentTool) map[string]any {
		t.Helper()
		var doc struct {
			Properties map[string]any `json:"properties"`
		}
		b, err := json.Marshal(st.Parameters())
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, &doc); err != nil {
			t.Fatal(err)
		}
		return doc.Properties
	}
	tree := agent.NewAgentTree("root", "m")
	root := tree.Node(tree.RootID())

	role, ok := props(newRoleScope(tree, scopeRoles()).spawnTool(fn, root))["role"].(map[string]any)
	if !ok {
		t.Fatal("spawn_agent lacks a role property")
	}
	if got := role["enum"]; !reflect.DeepEqual(got, []any{"reviewer", "writer"}) {
		t.Fatalf("role enum = %#v, want [reviewer writer]", got)
	}
	if _, ok := props(newRoleScope(tree, nil).spawnTool(fn, root))["role"]; ok {
		t.Fatal("spawn_agent advertises role with no roles defined")
	}
}
