package main

import (
	"context"
	"io"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	harnessv1 "github.com/ethanhinson/fuse/harness/v1"
	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/observe"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/probe"
	"github.com/ethanhinson/fuse/internal/runtime"
	"github.com/ethanhinson/fuse/internal/tools"
)

const testBrief = "You are a release-notes writer."

// instructionsTestBuild builds a root agent through buildAgentCore for a single
// model entry. permissions.mode is off so no scratch block is appended and the
// extra block reaches composition exactly as passed.
func instructionsTestBuild(t *testing.T, mc model.ModelConfig, extra, instructions string) *agent.Agent {
	t.Helper()
	cfg := config.Default()
	cfg.Permissions.Mode = "off"
	reg := model.NewRegistry("fast", map[string]model.ModelConfig{"fast": mc})
	a, _, err := buildAgentCore(cfg, reg, "fast", discardRenderer{}, extra, instructions, nil, "root",
		tools.NewRegistry(), permissions.AlwaysApprove, nil, false, nil, nil, observe.NoopObserver{})
	if err != nil {
		t.Fatalf("buildAgentCore: %v", err)
	}
	return a
}

// assertBriefOrder checks persona prompt < brief < extra block by position.
func assertBriefOrder(t *testing.T, sys, persona, brief, extra string) {
	t.Helper()
	p, b, e := strings.Index(sys, agent.PersonaPrompt(persona)), strings.Index(sys, brief), strings.Index(sys, extra)
	if p < 0 || b < 0 || e < 0 || !(p < b && b < e) {
		t.Fatalf("want persona < brief < extra, got positions %d, %d, %d in %q", p, b, e, sys)
	}
}

func TestBuildAgentCoreComposesInstructionsOnGatewayPath(t *testing.T) {
	a := instructionsTestBuild(t, model.ModelConfig{ID: "cloud/fast", Persona: "research"}, "EXTRA-BLOCK", testBrief)
	want := agent.ComposeSystemPrompt("research", "", testBrief, "EXTRA-BLOCK")
	if got := a.SystemPrompt(); got != want {
		t.Fatalf("system prompt:\n got: %q\nwant: %q", got, want)
	}
	assertBriefOrder(t, a.SystemPrompt(), "research", testBrief, "EXTRA-BLOCK")
}

func TestBuildAgentCoreComposesInstructionsOnCLIPath(t *testing.T) {
	t.Setenv("FUSE_HITL_MODE", "")
	a := instructionsTestBuild(t, model.ModelConfig{ID: "cli/claude", Persona: "research", SystemPrefix: "/no_think"}, "EXTRA-BLOCK", testBrief)
	if _, ok := completerOf(t, a).(*CLIAdapter); !ok {
		t.Fatalf("completer = %T, want *CLIAdapter", completerOf(t, a))
	}
	want := agent.ComposeSystemPrompt("research", "/no_think", testBrief, "EXTRA-BLOCK")
	if got := a.SystemPrompt(); got != want {
		t.Fatalf("system prompt:\n got: %q\nwant: %q", got, want)
	}
	assertBriefOrder(t, a.SystemPrompt(), "research", testBrief, "EXTRA-BLOCK")
}

func TestBuildAgentCoreEmptyInstructionsUnchanged(t *testing.T) {
	t.Setenv("FUSE_HITL_MODE", "")
	for _, id := range []string{"cloud/fast", "cli/claude"} {
		for _, ins := range []string{"", "  \n\t "} {
			a := instructionsTestBuild(t, model.ModelConfig{ID: id, Persona: "coding", SystemPrefix: "/no_think"}, "EXTRA-BLOCK", ins)
			want := agent.ComposeSystemPrompt("coding", "/no_think", "", "EXTRA-BLOCK")
			if got := a.SystemPrompt(); got != want {
				t.Errorf("%s instructions=%q: system prompt:\n got: %q\nwant: %q", id, ins, got, want)
			}
		}
	}
}

// defaultPersonaPrompt is the persona prompt of operatorYAML's default model.
func defaultPersonaPrompt(t *testing.T, operatorYAML string) string {
	t.Helper()
	cfg, err := config.FromYAML([]byte(operatorYAML), "operator")
	if err != nil {
		t.Fatal(err)
	}
	reg := registryFromConfig(cfg)
	mc, err := reg.Resolve(reg.Default)
	if err != nil {
		t.Fatal(err)
	}
	return agent.PersonaPrompt(mc.Persona)
}

// loopRootSystem starts one loop on rt and returns the system message of the
// first model request that loop made.
func loopRootSystem(t *testing.T, rt runtime.Runtime, gw *recordingGateway, spec *harnessv1.HarnessSpec) string {
	t.Helper()
	before := len(gw.snapshot())
	h, err := rt.StartLoop(context.Background(), runtime.LoopConfig{Task: "go", Harness: spec})
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	_, _ = h.Wait() // the turn cap ends the run; its error is not the point
	calls := gw.snapshot()
	if len(calls) <= before || len(calls[before].Messages) == 0 {
		t.Fatalf("loop made no model request")
	}
	m := calls[before].Messages[0]
	if m.Role != "system" {
		t.Fatalf("first message role = %q, want system", m.Role)
	}
	return m.Content
}

// assertInOrder checks every part occurs in sys, each after the previous one.
func assertInOrder(t *testing.T, sys string, parts ...string) {
	t.Helper()
	last := -1
	for _, p := range parts {
		i := strings.Index(sys, p)
		if i < 0 {
			t.Fatalf("system prompt lacks %q:\n%s", p, sys)
		}
		if i <= last {
			t.Fatalf("%q is out of order in the system prompt:\n%s", p, sys)
		}
		last = i
	}
}

// TestLoopServerSpecInstructionsReachRootPrompt is the composition-root proof
// on the production loop-server wiring: a spec's instructions reach the root's
// model-facing system message after the persona and before the skill listing.
func TestLoopServerSpecInstructionsReachRootPrompt(t *testing.T) {
	rt, gw, _ := harnessServer(t, harnessOperator)
	turns := uint32(1)
	sys := loopRootSystem(t, rt, gw, &harnessv1.HarnessSpec{MaxTurns: &turns, Instructions: testBrief})
	assertInOrder(t, sys, defaultPersonaPrompt(t, harnessOperator), testBrief, "- research")
}

// TestLoopServerOperatorInstructionsAndSpecOverride pins the layering: the
// operator's instructions apply to a loop without a spec and to a spec that
// leaves instructions unset, and a spec's instructions replace them.
func TestLoopServerOperatorInstructionsAndSpecOverride(t *testing.T) {
	op := harnessOperator + "instructions: OPERATOR-BRIEF\n"
	rt, gw, _ := harnessServer(t, op)
	turns := uint32(1)

	if sys := loopRootSystem(t, rt, gw, nil); !strings.Contains(sys, "OPERATOR-BRIEF") {
		t.Errorf("operator loop lacks the operator instructions:\n%s", sys)
	} else {
		assertInOrder(t, sys, defaultPersonaPrompt(t, op), "OPERATOR-BRIEF", "- research")
	}

	sys := loopRootSystem(t, rt, gw, &harnessv1.HarnessSpec{MaxTurns: &turns, Instructions: "SPEC-BRIEF"})
	if !strings.Contains(sys, "SPEC-BRIEF") || strings.Contains(sys, "OPERATOR-BRIEF") {
		t.Errorf("spec loop must carry SPEC-BRIEF and not OPERATOR-BRIEF:\n%s", sys)
	}

	sys = loopRootSystem(t, rt, gw, &harnessv1.HarnessSpec{MaxTurns: &turns, Skills: []*harnessv1.SkillRef{{Name: "research"}}})
	if !strings.Contains(sys, "OPERATOR-BRIEF") {
		t.Errorf("a spec without instructions must keep the operator's:\n%s", sys)
	}
}

// TestLoopServerBlankInstructionsMatchGolden: whitespace-only instructions
// leave the root system message byte for byte as it is today.
func TestLoopServerBlankInstructionsMatchGolden(t *testing.T) {
	rt, gw, _ := harnessServer(t, harnessOperator)
	turns := uint32(1)
	sys := loopRootSystem(t, rt, gw, &harnessv1.HarnessSpec{MaxTurns: &turns, Instructions: "  \n\t "})
	if dir := sessionScratchDir(); dir != "" {
		sys = strings.ReplaceAll(sys, dir, "<SCRATCH>")
	}
	want, err := os.ReadFile(loopRootPromptGolden)
	if err != nil {
		t.Fatal(err)
	}
	if sys != string(want) {
		t.Errorf("blank-instructions root prompt differs from golden:\n got: %q\nwant: %q", sys, string(want))
	}
}

// TestLoopServerSubagentsNeverGetInstructions: on the production loop-server
// wiring the root carries the spec's instructions while a plain subagent, a
// workflow lead and its workers never do.
func TestLoopServerSubagentsNeverGetInstructions(t *testing.T) {
	const brief = "ROOT-ONLY-BRIEF"
	spec := wfSpec()
	spec.Instructions = brief
	r := runHostedSpec(t, spec, func(task string, turn int, c gatewayCall) string {
		switch {
		case strings.Contains(task, "ROOT-TASK") && turn == 0:
			return toolCallsReply(
				call{name: "spawn_agent", args: map[string]any{"label": "helper", "task": "CHILD-TASK"}},
				call{name: "spawn_agent", args: map[string]any{"label": "lead", "task": "LEAD-TASK", "workflow": "wf"}},
			)
		case strings.Contains(task, "ROOT-TASK"):
			return textReply("root done")
		case strings.Contains(task, "CHILD-TASK"):
			return textReply("child done")
		}
		return leadRoute(task, turn, c)
	})

	root := turnRequest(t, r.gw, "ROOT-TASK", 0)
	if sys := root.Messages[0].Content; root.Messages[0].Role != "system" || !strings.Contains(sys, brief) {
		t.Fatalf("root system message lacks the instructions:\n%s", sys)
	}
	for _, marker := range []string{"CHILD-TASK", "LEAD-TASK", "READ-TASK"} {
		calls := r.gw.callsFor(marker)
		if len(calls) == 0 {
			t.Fatalf("no %s request: the subagent never ran", marker)
		}
		for i, c := range calls {
			if len(c.Messages) == 0 || c.Messages[0].Role != "system" {
				t.Fatalf("%s request %d has no system message", marker, i)
			}
			if strings.Contains(c.Messages[0].Content, brief) {
				t.Errorf("%s request %d system message carries the root's instructions:\n%s", marker, i, c.Messages[0].Content)
			}
		}
	}
}

// TestLocalRootsGetInstructionsChildrenDoNot covers the local bindings' deps
// builders: the root agent BuildAgent returns carries cfg.Instructions after
// the persona, and a subagent spawned through the binding's spawn_agent does not.
// The interactive shell's TUI build closure (shell.go) cannot run headless; it
// passes cfg.Instructions explicitly, like every other root build site.
func TestLocalRootsGetInstructionsChildrenDoNot(t *testing.T) {
	const brief = "LOCAL-BRIEF"
	for _, tc := range []struct {
		name string
		// build wires the binding's deps and returns them with the spawn_agent
		// arguments that reach a child under that binding.
		build func(t *testing.T, cfg config.Config, toolReg *tools.Registry, tree *agent.AgentTree) (runtime.Deps, map[string]any)
	}{
		{"one-shot", func(t *testing.T, cfg config.Config, toolReg *tools.Registry, tree *agent.AgentTree) (runtime.Deps, map[string]any) {
			reg := registryFromConfig(cfg)
			deps, closeMCP := buildOneShotRuntimeDeps(cfg, reg, reg.Default, toolReg, tree, io.Discard, false, nil,
				permissions.AlwaysApprove, "block", false, nil, nil)
			t.Cleanup(closeMCP)
			return deps, map[string]any{"label": "c", "task": "LOCAL-CHILD"}
		}},
		{"shell", func(t *testing.T, cfg config.Config, toolReg *tools.Registry, tree *agent.AgentTree) (runtime.Deps, map[string]any) {
			reg := registryFromConfig(cfg)
			return buildShellRuntimeDeps(shellDepsInput{
				cfg: cfg, reg: reg, alias: reg.Default, toolReg: toolReg, tree: tree,
				skillBlock: "block", childApprove: permissions.AlwaysApprove,
				rootApprove: permissions.AlwaysApprove,
			}), map[string]any{"label": "c", "task": "LOCAL-CHILD"}
		}},
		{"research-probe", func(t *testing.T, cfg config.Config, toolReg *tools.Registry, tree *agent.AgentTree) (runtime.Deps, map[string]any) {
			reg := registryFromConfig(cfg)
			scope := newWorkflowScope(tree, cfg.Workflows)
			return buildResearchProbeRuntimeDeps(researchProbeDepsInput{
				cfg: cfg, reg: reg, alias: reg.Default, toolReg: toolReg, tree: tree,
				scope: scope, logSink: probe.NewLog(),
			}), map[string]any{"label": "c", "task": "LOCAL-CHILD", "workflow": "research"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			gw := &routedGateway{route: func(string, int, gatewayCall) string { return textReply("ok") }}
			srv := httptest.NewServer(gw)
			t.Cleanup(srv.Close)
			cfg, err := config.FromYAML([]byte(wfOperatorYAML), "test")
			if err != nil {
				t.Fatal(err)
			}
			cfg.Gateway.URL, cfg.Gateway.Key = srv.URL, "tkn"
			cfg.Instructions = brief
			reg := registryFromConfig(cfg)
			toolReg := defaultToolRegistry(nil, cfg.Research, nil)
			tree := agent.NewAgentTreeWithConcurrency(reg.Default, reg.Default, cfg.Agents.MaxConcurrent)
			deps, spawnArgs := tc.build(t, cfg, toolReg, tree)

			a, _, _, err := deps.BuildAgent(nil, tree, reg.Default, toolReg)
			if err != nil {
				t.Fatalf("BuildAgent: %v", err)
			}
			mc, err := reg.Resolve(reg.Default)
			if err != nil {
				t.Fatal(err)
			}
			assertInOrder(t, a.SystemPrompt(), agent.PersonaPrompt(mc.Persona), brief)

			res := toolReg.Execute(context.Background(), "spawn_agent", argsJSON(spawnArgs))
			if res.IsError {
				t.Fatalf("spawn_agent: %s", res.Output)
			}
			calls := gw.callsFor("LOCAL-CHILD")
			if len(calls) == 0 {
				t.Fatal("no LOCAL-CHILD request: the child never ran")
			}
			for i, c := range calls {
				if len(c.Messages) == 0 || c.Messages[0].Role != "system" {
					t.Fatalf("child request %d has no system message", i)
				}
				if strings.Contains(c.Messages[0].Content, brief) {
					t.Errorf("child request %d carries the root's instructions:\n%s", i, c.Messages[0].Content)
				}
			}
		})
	}
}

// TestSpecDigestChangesWithInstructions: the production resolver's digest
// covers instructions, and an empty value is the same spec as an unset one.
func TestSpecDigestChangesWithInstructions(t *testing.T) {
	_, _, host := harnessServer(t, harnessOperator)
	digest := func(spec *harnessv1.HarnessSpec) string {
		t.Helper()
		res, err := host.resolver().Resolve(context.Background(), "", spec)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		return res.Digest
	}
	turns := uint32(1)
	base := digest(&harnessv1.HarnessSpec{MaxTurns: &turns})
	if got := digest(&harnessv1.HarnessSpec{MaxTurns: &turns, Instructions: ""}); got != base {
		t.Errorf("empty instructions changed the digest: %s != %s", got, base)
	}
	a := digest(&harnessv1.HarnessSpec{MaxTurns: &turns, Instructions: "brief A"})
	b := digest(&harnessv1.HarnessSpec{MaxTurns: &turns, Instructions: "brief B"})
	if a == base || b == base || a == b {
		t.Errorf("digests must differ by instructions: base %s, A %s, B %s", base, a, b)
	}
}
