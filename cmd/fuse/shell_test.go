package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/observe"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/tui"
)

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// buildAgentWithRendererAndTrace must resolve a known alias and return a usable agent
// bound to the injected renderer.
func TestBuildAgentWithRenderer(t *testing.T) {
	cfg := config.Default()
	reg := model.DefaultRegistry()
	toolReg := defaultToolRegistry(nil, cfg.Research, nil)
	r := tui.NewRenderer(discard{}, false)

	a, err := buildAgentWithRendererAndTrace(cfg, reg, reg.Default, r, false, "block", "", toolReg, permissions.AlwaysApprove, nil, "", nil, false, nil, nil, observe.NoopObserver{})
	if err != nil {
		t.Fatalf("buildAgentWithRenderer: %v", err)
	}
	if a == nil {
		t.Fatal("expected a non-nil agent")
	}
}

// TestBuildAgentLoopApprovalWiring verifies the doom-loop force-through hook is
// wired only in the interactive posture: interactive builds set LoopApproval,
// headless builds leave it nil (a trip aborts). See change 0038.
func TestBuildAgentLoopApprovalWiring(t *testing.T) {
	cfg := config.Default()
	reg := model.DefaultRegistry()
	toolReg := defaultToolRegistry(nil, cfg.Research, nil)
	r := tui.NewRenderer(discard{}, false)

	interactive, _, err := buildAgentCore(cfg, reg, reg.Default, r, "", "", nil, "root", toolReg, permissions.AlwaysApprove, nil, true, nil, nil, observe.NoopObserver{})
	if err != nil {
		t.Fatalf("interactive build: %v", err)
	}
	if interactive.LoopApproval == nil {
		t.Error("interactive agent should have LoopApproval wired")
	}

	headless, _, err := buildAgentCore(cfg, reg, reg.Default, r, "", "", nil, "root", toolReg, permissions.AlwaysApprove, nil, false, nil, nil, observe.NoopObserver{})
	if err != nil {
		t.Fatalf("headless build: %v", err)
	}
	if headless.LoopApproval != nil {
		t.Error("headless agent should NOT have LoopApproval wired (a trip aborts)")
	}
}

// TestBuildAgentTurnCapAtCompositionRoot asserts the turn cap at the builders
// themselves: an unset max_turns is unlimited in every posture, an explicit one
// caps every posture.
func TestBuildAgentTurnCapAtCompositionRoot(t *testing.T) {
	reg := model.DefaultRegistry()
	r := tui.NewRenderer(discard{}, false)
	seven := 7
	for _, tc := range []struct {
		name string
		max  *int
		want int
	}{
		{"unset", nil, 0},
		{"explicit 7", &seven, 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.MaxTurns = tc.max
			toolReg := defaultToolRegistry(nil, cfg.Research, nil)
			for _, interactive := range []bool{false, true} {
				a, _, err := buildAgentCore(cfg, reg, reg.Default, r, "", "", nil, "root", toolReg, permissions.AlwaysApprove, nil, interactive, nil, nil, observe.NoopObserver{})
				if err != nil {
					t.Fatalf("buildAgentCore(interactive=%v): %v", interactive, err)
				}
				if got := a.MaxTurns(); got != tc.want {
					t.Errorf("buildAgentCore(interactive=%v) MaxTurns = %d, want %d", interactive, got, tc.want)
				}
			}
			child, err := buildChildAgent(cfg, reg, reg.Default, r, "sys", toolReg.Clone(), permissions.AlwaysApprove, nil, "child", nil, false, nil, nil)
			if err != nil {
				t.Fatalf("buildChildAgent: %v", err)
			}
			if got := child.MaxTurns(); got != tc.want {
				t.Errorf("buildChildAgent MaxTurns = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestBuildAgentWithRendererUnknownAlias(t *testing.T) {
	cfg := config.Default()
	reg := model.DefaultRegistry()
	toolReg := defaultToolRegistry(nil, cfg.Research, nil)
	r := tui.NewRenderer(discard{}, false)
	if _, err := buildAgentWithRendererAndTrace(cfg, reg, "no-such-model", r, false, "", "", toolReg, permissions.AlwaysApprove, nil, "", nil, false, nil, nil, observe.NoopObserver{}); err == nil {
		t.Fatal("expected error for unknown alias")
	}
}

// The runShell-side builder closure must satisfy tui.AgentBuilder and, when
// wired into a ShellModel, produce a view that starts on the requested alias.
func TestShellModelBuilderWiring(t *testing.T) {
	cfg := config.Default()
	reg := model.DefaultRegistry()
	toolReg := defaultToolRegistry(nil, cfg.Research, nil)
	var build tui.AgentBuilder = func(alias string, r agent.Renderer, approve permissions.ApprovalFunc) (*agent.Agent, error) {
		return buildAgentWithRendererAndTrace(cfg, reg, alias, r, false, "", "", toolReg, permissions.Human(approve), nil, "", nil, false, nil, nil, observe.NoopObserver{})
	}
	m := tui.NewShellModel(reg.Default, false, "dark", reg, nil, build, permissions.NewSessionMode(permissions.ModeSmart), true)
	next, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	view := next.(tui.ShellModel).View()
	if !strings.Contains(view, reg.Default) {
		t.Errorf("view missing alias %q: %q", reg.Default, view)
	}
}
