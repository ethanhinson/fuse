package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/model"
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
	toolReg := defaultToolRegistry(cfg.Research, nil)
	r := tui.NewRenderer(discard{}, false)

	a, err := buildAgentWithRendererAndTrace(cfg, reg, reg.Default, r, false, "block", toolReg, permissions.AlwaysApprove, nil, "", nil)
	if err != nil {
		t.Fatalf("buildAgentWithRenderer: %v", err)
	}
	if a == nil {
		t.Fatal("expected a non-nil agent")
	}
}

func TestBuildAgentWithRendererUnknownAlias(t *testing.T) {
	cfg := config.Default()
	reg := model.DefaultRegistry()
	toolReg := defaultToolRegistry(cfg.Research, nil)
	r := tui.NewRenderer(discard{}, false)
	if _, err := buildAgentWithRendererAndTrace(cfg, reg, "no-such-model", r, false, "", toolReg, permissions.AlwaysApprove, nil, "", nil); err == nil {
		t.Fatal("expected error for unknown alias")
	}
}

// The runShell-side builder closure must satisfy tui.AgentBuilder and, when
// wired into a ShellModel, produce a view that starts on the requested alias.
func TestShellModelBuilderWiring(t *testing.T) {
	cfg := config.Default()
	reg := model.DefaultRegistry()
	toolReg := defaultToolRegistry(cfg.Research, nil)
	var build tui.AgentBuilder = func(alias string, r agent.Renderer, approve permissions.ApprovalFunc) (*agent.Agent, error) {
		return buildAgentWithRendererAndTrace(cfg, reg, alias, r, false, "", toolReg, approve, nil, "", nil)
	}
	m := tui.NewShellModel(reg.Default, false, "dark", reg, nil, build)
	next, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	view := next.(tui.ShellModel).View()
	if !strings.Contains(view, reg.Default) {
		t.Errorf("view missing alias %q: %q", reg.Default, view)
	}
}
