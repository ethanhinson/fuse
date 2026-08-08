package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/ethanhinson/fuse/internal/agent"
)

// TestBlackboardTabRendersEntries: pressing "b" in the agents overlay opens the
// Blackboard section, which renders each key, its JSON value, and a wrote-by
// indicator from the writer's label.
func TestBlackboardTabRendersEntries(t *testing.T) {
	// Force TrueColor so styled output renders deterministically for View capture.
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)

	tree := agent.NewAgentTree("root", "m")
	root := tree.Node(tree.RootID())
	bb := agent.NewBlackboard(tree)
	bb.Put("plan/outline", map[string]any{"steps": float64(3)}, root.ID, "root")
	bb.Put("facet/a", "done", "child-1", "research/facet-2")

	m := NewAgentsModel(tree, nil).WithBlackboard(bb)
	m.width, m.height = 120, 30

	// Enter the blackboard section.
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'b'}})
	view := m.View()

	if !strings.Contains(view, "plan/outline") {
		t.Errorf("blackboard view should show key plan/outline\n%s", view)
	}
	if !strings.Contains(view, "facet/a") {
		t.Errorf("blackboard view should show key facet/a\n%s", view)
	}
	if !strings.Contains(view, `"steps":3`) {
		t.Errorf("blackboard view should show JSON value with steps:3\n%s", view)
	}
	if !strings.Contains(view, "research/facet-2") {
		t.Errorf("blackboard view should show wrote-by label research/facet-2\n%s", view)
	}
}

// TestBlackboardTabSanitizesAndWraps: an oversized/untrusted value (ESC bytes,
// a very long line) is sanitized (no raw control bytes) and hard-wrapped so no
// rendered line exceeds the pane width.
func TestBlackboardTabSanitizesAndWraps(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)

	tree := agent.NewAgentTree("root", "m")
	root := tree.Node(tree.RootID())
	bb := agent.NewBlackboard(tree)
	// A string value containing an ESC byte and a very long unbroken run.
	nasty := "\x1b[31m" + strings.Repeat("A", 500) + "\x07tail"
	bb.Put("evil", nasty, root.ID, "root")

	m := NewAgentsModel(tree, nil).WithBlackboard(bb)
	m.width, m.height = 100, 30
	detailW := m.width - m.width*40/100 - 1

	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'b'}})
	view := m.View()

	// No raw ESC / BEL control bytes survive.
	if strings.ContainsRune(view, '\x1b') {
		// lipgloss styling emits ESC sequences; only the untrusted payload's raw
		// ESC should be gone. Check the specific injected sequence is absent.
		if strings.Contains(view, "\x1b[31m") {
			t.Error("raw ESC color sequence from untrusted value leaked into view")
		}
	}
	if strings.ContainsRune(view, '\x07') {
		t.Error("raw BEL byte from untrusted value leaked into view")
	}

	// No rendered line exceeds the pane width (each row is treeW+1+detailW).
	for _, line := range strings.Split(view, "\n") {
		// The detail column is the segment after the divider; check the whole row
		// stays within the overlay width.
		if lipgloss.Width(line) > m.width {
			t.Errorf("line exceeds overlay width %d: got %d", m.width, lipgloss.Width(line))
		}
	}
	_ = detailW
}
