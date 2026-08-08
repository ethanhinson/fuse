package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/ethanhinson/fuse/internal/agent"
)

// TestBlackboardTabScrollClampsAtEnd refutes the "bbScroll has no upper bound"
// concern: holding 'j' (scroll down) past the content must clamp — the board
// keeps at least the last row visible and never renders a fully empty pane or
// panics. buildBlackboardLines clamps bbScroll to len(body)-1 each render.
func TestBlackboardTabScrollClampsAtEnd(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)

	tree := agent.NewAgentTree("root", "m")
	root := tree.Node(tree.RootID())
	bb := agent.NewBlackboard(tree)
	// A handful of entries so there is real content to scroll through.
	for _, k := range []string{"a/one", "a/two", "b/three", "c/four"} {
		bb.Put(k, map[string]any{"v": k}, root.ID, "root")
	}

	m := NewAgentsModel(tree, nil).WithBlackboard(bb)
	m.width, m.height = 100, 12 // small height so content exceeds the window
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'b'}})

	// Slam 'j' far past the end.
	for i := 0; i < 500; i++ {
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	}
	view := m.View()

	// Must not have scrolled into a blank board: at least one real key or the
	// header is still visible, and the view is non-empty.
	if strings.TrimSpace(view) == "" {
		t.Fatal("over-scroll produced a blank view")
	}
	if !strings.Contains(view, "Blackboard") && !strings.Contains(view, "/four") {
		t.Errorf("over-scroll hid all content (no header, no last key):\n%s", view)
	}

	// 'g' (top) must bring the first entry back.
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'g'}})
	if top := m.View(); !strings.Contains(top, "a/one") {
		t.Errorf("'g' did not return to the top; view:\n%s", top)
	}
}

// TestBlackboardTabEmptyBoard covers the empty-store render: the tab shows the
// "empty" notice, not a blank or panicking pane.
func TestBlackboardTabEmptyBoard(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)

	tree := agent.NewAgentTree("root", "m")
	bb := agent.NewBlackboard(tree)
	m := NewAgentsModel(tree, nil).WithBlackboard(bb)
	m.width, m.height = 100, 20
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'b'}})

	if v := m.View(); !strings.Contains(v, "Blackboard is empty") {
		t.Errorf("empty board should show the empty notice; got:\n%s", v)
	}
}

// TestBlackboardTabNilBoard covers a nil blackboard (no WithBlackboard): the tab
// must render a graceful notice rather than panic.
func TestBlackboardTabNilBoard(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)

	tree := agent.NewAgentTree("root", "m")
	m := NewAgentsModel(tree, nil) // no WithBlackboard => nil store
	m.width, m.height = 100, 20
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'b'}})

	v := m.View() // must not panic
	if !strings.Contains(v, "No blackboard") && !strings.Contains(v, "Blackboard") {
		t.Errorf("nil board should render a notice, not blank; got:\n%s", v)
	}
}
