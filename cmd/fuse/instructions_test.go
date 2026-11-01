package main

import (
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/observe"
	"github.com/ethanhinson/fuse/internal/permissions"
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
