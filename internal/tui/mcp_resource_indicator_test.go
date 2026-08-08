package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/permissions"
)

// newIndicatorModel builds a minimal sized ShellModel for indicator rendering.
func newIndicatorModel(t *testing.T) ShellModel {
	t.Helper()
	m := NewShellModel("alpha", false, "", model.DefaultRegistry(), nil, nil, permissions.NewSessionMode(permissions.ModeSmart), true)
	// Size the viewport so View() renders the full chrome (status line included).
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	return nm.(ShellModel)
}

// TestMCPResourceUpdatedRendersStaleIndicator: an MCPResourceUpdatedMsg delivered
// to the model records the URI stale and View() renders a stale/updated
// indicator naming the resource.
func TestMCPResourceUpdatedRendersStaleIndicator(t *testing.T) {
	m := newIndicatorModel(t)

	// No indicator before any update.
	if strings.Contains(stripANSIString(m.View()), "stale") {
		t.Fatalf("indicator present before any resource update:\n%s", m.View())
	}

	nm, _ := m.Update(MCPResourceUpdatedMsg{Server: "fuse", URI: "fuse://tools"})
	m = nm.(ShellModel)

	frame := stripANSIString(m.View())
	if !strings.Contains(frame, "fuse://tools") {
		t.Errorf("View() should name the stale resource, got:\n%s", frame)
	}
	if !strings.Contains(strings.ToLower(frame), "stale") && !strings.Contains(strings.ToLower(frame), "updated") {
		t.Errorf("View() should show a stale/updated indicator, got:\n%s", frame)
	}
}

func stripANSIString(s string) string { return string(stripANSI([]byte(s))) }
