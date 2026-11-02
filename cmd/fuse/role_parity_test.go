package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	harnessv1 "github.com/ethanhinson/fuse/harness/v1"
	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/probe"
	"github.com/ethanhinson/fuse/internal/tools"
)

// Role parity (change 0114): every cloned child-builder site builds a role
// spawn the same way. The role brief lands in the child's system prompt after
// the persona, the role's tool set (plus the blackboard tools) replaces the
// child's tools, the role's model pin beats the parent's model, and a model the
// call names beats the role's pin. A role with no tool set inherits exactly
// what a plain child gets.

// roleParityRoles is the roles block every row configures.
const roleParityRoles = `roles:
  reviewer:
    description: Reviews code for correctness.
    instructions: REVIEWER-BRIEF
    tools: [read_file, grep]
    model: house
  writer:
    description: Writes prose.
    instructions: WRITER-BRIEF
`

// roleOperatorYAML is wfOperatorYAML with the two parity roles.
const roleOperatorYAML = wfOperatorYAML + roleParityRoles

// roleParitySpawns are the spawn_agent calls every row issues from its root:
// a reviewer, a reviewer whose call names a model, a writer and a plain child.
var roleParitySpawns = []map[string]any{
	{"label": "rev", "task": "REV-TASK", "role": "reviewer"},
	{"label": "rv2", "task": "RV2-TASK", "role": "reviewer", "model": "fast"},
	{"label": "wri", "task": "WRI-TASK", "role": "writer"},
	{"label": "pln", "task": "PLN-TASK"},
}

// roleParitySpec is a harness spec defining the same two roles.
func roleParitySpec() *harnessv1.HarnessSpec {
	return &harnessv1.HarnessSpec{Roles: map[string]*harnessv1.Role{
		"reviewer": {Description: "Reviews code for correctness.", Instructions: "REVIEWER-BRIEF", Tools: []string{"read_file", "grep"}, Model: "house"},
		"writer":   {Description: "Writes prose.", Instructions: "WRITER-BRIEF"},
	}}
}

// roleChildRoute answers every child with one line; root turn 0 (hosted rows
// only) issues the parity spawns in parallel.
func roleChildRoute(task string, turn int, _ gatewayCall) string {
	if strings.Contains(task, "ROOT-TASK") && turn == 0 {
		var cs []call
		for _, a := range roleParitySpawns {
			cs = append(cs, call{name: "spawn_agent", args: a})
		}
		return toolCallsReply(cs...)
	}
	return textReply("done")
}

// personaPromptFor is the persona prompt of alias under operatorYAML.
func personaPromptFor(t *testing.T, operatorYAML, alias string) string {
	t.Helper()
	cfg, err := config.FromYAML([]byte(operatorYAML), "operator")
	if err != nil {
		t.Fatal(err)
	}
	mc, err := registryFromConfig(cfg).Resolve(alias)
	if err != nil {
		t.Fatal(err)
	}
	return agent.PersonaPrompt(mc.Persona)
}

// roleParityLocal runs one local binding: build wires the binding's deps, the
// root is built through deps.BuildAgent, and each parity spawn executes on the
// root's spawn_agent.
func roleParityLocal(t *testing.T, build func(t *testing.T, cfg config.Config, reg *model.Registry, toolReg *tools.Registry, tree *agent.AgentTree) (buildRoot func() error)) (*routedGateway, *agent.AgentTree) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	gw := &routedGateway{route: roleChildRoute}
	srv := httptest.NewServer(gw)
	t.Cleanup(srv.Close)
	cfg, err := config.FromYAML([]byte(roleOperatorYAML), "test")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Gateway.URL, cfg.Gateway.Key = srv.URL, "tkn"
	reg := registryFromConfig(cfg)
	toolReg := defaultToolRegistry(nil, cfg.Research, nil)
	tree := agent.NewAgentTreeWithConcurrency(reg.Default, reg.Default, cfg.Agents.MaxConcurrent)
	if err := build(t, cfg, reg, toolReg, tree)(); err != nil {
		t.Fatalf("BuildAgent: %v", err)
	}
	for _, a := range roleParitySpawns {
		if res := toolReg.Execute(context.Background(), "spawn_agent", argsJSON(a)); res.IsError {
			t.Fatalf("spawn_agent %v: %s", a, res.Output)
		}
	}
	return gw, tree
}

// roleParityRow is one child-builder site (and, for the loop server, one
// source of role config).
type roleParityRow struct {
	name string
	// operatorYAML is the config whose models resolve the persona prompts.
	operatorYAML string
	run          func(t *testing.T) (*routedGateway, *agent.AgentTree)
}

func roleParityRows() []roleParityRow {
	return []roleParityRow{
		{"one-shot", roleOperatorYAML, func(t *testing.T) (*routedGateway, *agent.AgentTree) {
			return roleParityLocal(t, func(t *testing.T, cfg config.Config, reg *model.Registry, toolReg *tools.Registry, tree *agent.AgentTree) func() error {
				deps, closeMCP := buildOneShotRuntimeDeps(cfg, reg, reg.Default, toolReg, tree, io.Discard, false, nil,
					permissions.AlwaysApprove, "block", false, nil, nil)
				t.Cleanup(closeMCP)
				return func() error { _, _, _, err := deps.BuildAgent(nil, tree, reg.Default, toolReg); return err }
			})
		}},
		{"shell", roleOperatorYAML, func(t *testing.T) (*routedGateway, *agent.AgentTree) {
			return roleParityLocal(t, func(t *testing.T, cfg config.Config, reg *model.Registry, toolReg *tools.Registry, tree *agent.AgentTree) func() error {
				deps := buildShellRuntimeDeps(shellDepsInput{
					cfg: cfg, reg: reg, alias: reg.Default, toolReg: toolReg, tree: tree,
					skillBlock: "block", childApprove: permissions.AlwaysApprove,
					rootApprove: permissions.AlwaysApprove,
				})
				return func() error { _, _, _, err := deps.BuildAgent(nil, tree, reg.Default, toolReg); return err }
			})
		}},
		{"research-probe", roleOperatorYAML, func(t *testing.T) (*routedGateway, *agent.AgentTree) {
			return roleParityLocal(t, func(t *testing.T, cfg config.Config, reg *model.Registry, toolReg *tools.Registry, tree *agent.AgentTree) func() error {
				deps := buildResearchProbeRuntimeDeps(researchProbeDepsInput{
					cfg: cfg, reg: reg, alias: reg.Default, toolReg: toolReg, tree: tree,
					scope: newWorkflowScope(tree, cfg.Workflows, cfg.Roles), logSink: probe.NewLog(),
				})
				return func() error { _, _, _, err := deps.BuildAgent(nil, tree, reg.Default, toolReg); return err }
			})
		}},
		{"loop-server/operator", hostedOperatorYAML + roleParityRoles, func(t *testing.T) (*routedGateway, *agent.AgentTree) {
			// An empty spec resolves to the operator config (and its roles)
			// through the same resolver a spec does.
			r := runHostedSpecWith(t, hostedOperatorYAML+roleParityRoles, &harnessv1.HarnessSpec{}, roleChildRoute)
			return r.gw, r.tree
		}},
		{"loop-server/spec", hostedOperatorYAML, func(t *testing.T) (*routedGateway, *agent.AgentTree) {
			r := runHostedSpec(t, roleParitySpec(), roleChildRoute)
			return r.gw, r.tree
		}},
	}
}

// TestRoleParityAcrossChildBuilders drives the parity spawns through every
// child-builder site and checks each one builds the role children alike.
func TestRoleParityAcrossChildBuilders(t *testing.T) {
	reviewerTools := []string{"blackboard_delete", "blackboard_keys", "blackboard_read", "blackboard_wait", "blackboard_write", "grep", "read_file"}
	for _, row := range roleParityRows() {
		t.Run(row.name, func(t *testing.T) {
			gw, tree := row.run(t)
			house := personaPromptFor(t, row.operatorYAML, "house")
			fast := personaPromptFor(t, row.operatorYAML, "fast")

			system := func(marker string) (gatewayCall, string) {
				t.Helper()
				c := turnRequest(t, gw, marker, 0)
				if len(c.Messages) == 0 || c.Messages[0].Role != "system" {
					t.Fatalf("%s: first message is not the system prompt", marker)
				}
				return c, c.Messages[0].Content
			}

			// Reviewer: role brief after the persona, role model pin over the
			// parent's model, role tool set plus the blackboard tools.
			rev, revSys := system("REV-TASK")
			assertInOrder(t, revSys, house, "REVIEWER-BRIEF")
			if rev.Model != "cloud/house" {
				t.Errorf("REV-TASK model = %q, want the role's pin cloud/house", rev.Model)
			}
			if got := sortedToolNames(rev); !reflect.DeepEqual(got, reviewerTools) {
				t.Errorf("REV-TASK tools = %v, want %v", got, reviewerTools)
			}

			// A model the call names beats the role's pin; the brief still applies.
			rv2, rv2Sys := system("RV2-TASK")
			if rv2.Model != "cloud/fast" {
				t.Errorf("RV2-TASK model = %q, want the call's model cloud/fast over the role's pin", rv2.Model)
			}
			assertInOrder(t, rv2Sys, fast, "REVIEWER-BRIEF")

			// Writer: brief after the persona, the parent's model, and exactly
			// the plain child's tool set.
			wri, wriSys := system("WRI-TASK")
			assertInOrder(t, wriSys, fast, "WRITER-BRIEF")
			if wri.Model != "cloud/fast" {
				t.Errorf("WRI-TASK model = %q, want the parent's cloud/fast", wri.Model)
			}
			pln, plnSys := system("PLN-TASK")
			if got, want := sortedToolNames(wri), sortedToolNames(pln); !reflect.DeepEqual(got, want) {
				t.Errorf("WRI-TASK tools = %v, want the plain child's %v", got, want)
			}
			if strings.Contains(plnSys, "REVIEWER-BRIEF") || strings.Contains(plnSys, "WRITER-BRIEF") {
				t.Errorf("PLN-TASK system prompt carries a role brief:\n%s", plnSys)
			}
			if strings.Contains(wriSys, "REVIEWER-BRIEF") || strings.Contains(revSys, "WRITER-BRIEF") {
				t.Errorf("a role child carries another role's brief")
			}

			for label, want := range map[string]string{"rev": "reviewer", "rv2": "reviewer", "wri": "writer", "pln": ""} {
				if got := nodeLabelled(t, tree, label).Role; got != want {
					t.Errorf("node %q Role = %q, want %q", label, got, want)
				}
			}
		})
	}
}

// TestRoleParityTableCoversEveryChildBuilder keeps the parity table in step
// with the code: one row per child-builder site, plus one for the loop
// server's second role source (operator config and harness spec).
func TestRoleParityTableCoversEveryChildBuilder(t *testing.T) {
	count := 0
	for _, src := range nonTestGoFiles(t) {
		count += strings.Count(src, "agent.WithChildBuilder(")
	}
	if count > 4 {
		t.Fatalf("agent.WithChildBuilder( appears %d times: a new child-builder site exists; add a row to the role parity table", count)
	}
	if rows := len(roleParityRows()); rows != count+1 {
		t.Fatalf("role parity table has %d rows, want %d (one per child-builder site, plus the loop server's spec source)", rows, count+1)
	}
}

// TestRoleBriefCompositionByteForByte pins where a role brief lands: exactly
// the instructions slot of agent.ComposeSystemPrompt, on the gateway and the
// CLI paths.
func TestRoleBriefCompositionByteForByte(t *testing.T) {
	t.Setenv("FUSE_HITL_MODE", "")
	for _, tc := range []struct {
		id, prefix string
	}{{"cloud/fast", ""}, {"cli/claude", "/no_think"}} {
		a := instructionsTestBuild(t, model.ModelConfig{ID: tc.id, Persona: "research", SystemPrefix: tc.prefix}, "EXTRA-BLOCK", "REVIEWER-BRIEF")
		want := agent.ComposeSystemPrompt("research", tc.prefix, "REVIEWER-BRIEF", "EXTRA-BLOCK")
		if got := a.SystemPrompt(); got != want {
			t.Errorf("%s system prompt:\n got: %q\nwant: %q", tc.id, got, want)
		}
	}
}

// TestNoRolesSpawnSchemaUnchanged: with no roles configured the root's
// spawn_agent schema has no role parameter and is byte-identical to the tool
// built without roles.
func TestNoRolesSpawnSchemaUnchanged(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg, err := config.FromYAML([]byte(wfOperatorYAML), "test")
	if err != nil {
		t.Fatal(err)
	}
	reg := registryFromConfig(cfg)
	toolReg := defaultToolRegistry(nil, cfg.Research, nil)
	tree := agent.NewAgentTreeWithConcurrency(reg.Default, reg.Default, cfg.Agents.MaxConcurrent)
	deps, closeMCP := buildOneShotRuntimeDeps(cfg, reg, reg.Default, toolReg, tree, io.Discard, false, nil,
		permissions.AlwaysApprove, "block", false, nil, nil)
	t.Cleanup(closeMCP)
	if _, _, _, err := deps.BuildAgent(nil, tree, reg.Default, toolReg); err != nil {
		t.Fatalf("BuildAgent: %v", err)
	}
	var st tools.Tool
	for _, tl := range toolReg.Tools() {
		if tl.Name() == "spawn_agent" {
			st = tl
		}
	}
	if st == nil {
		t.Fatal("root registry has no spawn_agent")
	}
	params := st.Parameters()
	if props, _ := params["properties"].(map[string]any); props == nil {
		t.Fatalf("spawn_agent schema has no properties: %v", params)
	} else if _, has := props["role"]; has {
		t.Fatalf("spawn_agent offers role with no roles configured: %v", props["role"])
	}
	got, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(tools.NewSpawnAgentToolWithBudget(nil, nil).WithWorkflows(sortedWorkflowNames(cfg.Workflows)).Parameters())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("spawn_agent schema with no roles:\n got: %s\nwant: %s", got, want)
	}
}
