package main

import (
	"context"
	"io"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

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
	return instructionsTestBuildCfg(t, cfg, mc, extra, instructions)
}

// instructionsTestBuildCfg is instructionsTestBuild with a caller-built config.
func instructionsTestBuildCfg(t *testing.T, cfg config.Config, mc model.ModelConfig, extra, instructions string) *agent.Agent {
	t.Helper()
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
func loopRootSystem(t *testing.T, rt runtime.Runtime, gw *recordingGateway) string {
	t.Helper()
	before := len(gw.snapshot())
	h, err := rt.StartLoop(context.Background(), runtime.LoopConfig{Task: "go"})
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

// TestInstructionsReachRootPrompt is the composition-root proof on the
// production one-shot wiring: the config's instructions reach the root's
// model-facing system message after the persona and before the skill listing.
func TestInstructionsReachRootPrompt(t *testing.T) {
	op := harnessOperator + "instructions: OPERATOR-BRIEF\n"
	rt, gw := harnessServer(t, op)
	sys := loopRootSystem(t, rt, gw)
	assertInOrder(t, sys, defaultPersonaPrompt(t, op), "OPERATOR-BRIEF", "- research")
}

// TestBlankInstructionsMatchGolden: whitespace-only instructions
// leave the root system message byte for byte as it is today.
func TestBlankInstructionsMatchGolden(t *testing.T) {
	rt, gw := harnessServer(t, harnessOperator+"instructions: \"  \\n\\t \"\n")
	sys := loopRootSystem(t, rt, gw)
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

// TestSubagentsNeverGetInstructions: on the production one-shot wiring the root
// carries the config's instructions while a subagent and its own child never do.
func TestSubagentsNeverGetInstructions(t *testing.T) {
	const brief = "ROOT-ONLY-BRIEF"
	r := runHostedConfig(t, hostedOperatorYAML+"instructions: "+brief+"\n", func(task string, turn int, c gatewayCall) string {
		switch {
		case strings.Contains(task, "ROOT-TASK") && turn == 0:
			return toolCallsReply(
				call{name: "spawn_agent", args: map[string]any{"label": "helper", "task": "CHILD-TASK"}},
				call{name: "spawn_agent", args: map[string]any{"label": "lead", "task": "LEAD-TASK"}},
			)
		case strings.Contains(task, "ROOT-TASK"):
			return textReply("root done")
		case strings.Contains(task, "LEAD-TASK") && turn == 0:
			return toolCallsReply(call{name: "spawn_agent", args: map[string]any{"label": "r0", "task": "READ-TASK"}})
		case strings.Contains(task, "LEAD-TASK"):
			return textReply("lead done")
		}
		return textReply("child done")
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
		{"research-probe", func(t *testing.T, cfg config.Config, toolReg *tools.Registry, tree *agent.AgentTree) (runtime.Deps, map[string]any) {
			reg := registryFromConfig(cfg)
			return buildResearchProbeRuntimeDeps(researchProbeDepsInput{
				cfg: cfg, reg: reg, alias: reg.Default, toolReg: toolReg, tree: tree,
				logSink: probe.NewLog(),
			}), map[string]any{"label": "c", "task": "LOCAL-CHILD"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			gw := &routedGateway{route: func(string, int, gatewayCall) string { return textReply("ok") }}
			srv := httptest.NewServer(gw)
			t.Cleanup(srv.Close)
			cfg, err := config.FromYAML([]byte(hostedOperatorYAML), "test")
			if err != nil {
				t.Fatal(err)
			}
			cfg.Gateway.URL, cfg.Gateway.Key = srv.URL, "tkn"
			cfg.Instructions = brief
			reg := registryFromConfig(cfg)
			toolReg := defaultToolRegistry(nil, cfg.Research, nil)
			tree := agent.NewAgentTreeWithConcurrency(reg.Default, reg.Default, cfg.Agents.MaxConcurrent)
			deps, spawnArgs := tc.build(t, cfg, toolReg, tree)

			a, err := deps.BuildAgent(nil, tree, reg.Default, toolReg)
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

// TestBuildAgentCoreToolLessPrompt: a tool-less config composes the one-line
// tool-less persona plus the brief, and drops every tool-driven extra block.
// A cli/ model refuses a tool-less config outright (TestNoToolsRejectsCLIModel),
// so only gateway paths compose a tool-less prompt.
func TestBuildAgentCoreToolLessPrompt(t *testing.T) {
	t.Setenv("FUSE_HITL_MODE", "")
	for _, tc := range []struct{ id, prefix string }{{"cloud/fast", ""}, {"cloud/thinker", "/no_think"}} {
		cfg := config.Default()
		cfg.NoTools = true // permissions mode left at its default, so the scratch block would otherwise be appended
		a := instructionsTestBuildCfg(t, cfg, model.ModelConfig{ID: tc.id, Persona: "research", SystemPrefix: tc.prefix}, spawnAgentBlock+"EXTRA-BLOCK", "BRIEF")
		want := agent.ComposeToolLessSystemPrompt("research", tc.prefix, "BRIEF")
		if got := a.SystemPrompt(); got != want {
			t.Errorf("%s:\n got: %q\nwant: %q", tc.id, got, want)
		}
	}
}
