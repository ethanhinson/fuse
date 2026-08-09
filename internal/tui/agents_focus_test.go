package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/ethanhinson/fuse/internal/agent"
)

// TestFocusRightFocused verifies the derived rightFocused() helper tracks the
// existing state flags: false on the fresh tree view, true after entering each
// right-pane sub-view, and false again after backing out to the tree.
func TestFocusRightFocused(t *testing.T) {
	tree := agent.NewAgentTree("root", "m")
	root := tree.Node(tree.RootID())
	bb := agent.NewBlackboard(tree)
	bb.Put("k", "v", root.ID, "root")

	m := NewAgentsModel(tree, nil).WithBlackboard(bb)
	m.width, m.height = 120, 30

	if m.rightFocused() {
		t.Fatal("fresh tree view should not be right-focused")
	}

	// tab → detail
	m.Update(tea.KeyMsg{Type: tea.KeyTab})
	if !m.inDetail || !m.rightFocused() {
		t.Fatalf("after tab, detail should be right-focused: inDetail=%v", m.inDetail)
	}
	// tab back → tree
	m.Update(tea.KeyMsg{Type: tea.KeyTab})
	if m.rightFocused() {
		t.Fatal("after tab back, tree should be left-focused")
	}

	// b → blackboard
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'b'}})
	if !m.inBlackboard || !m.rightFocused() {
		t.Fatalf("after b, blackboard should be right-focused: inBlackboard=%v", m.inBlackboard)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.rightFocused() {
		t.Fatal("after esc from blackboard, tree should be left-focused")
	}

	// event view
	m.inEventView = true
	if !m.rightFocused() {
		t.Fatal("event view should be right-focused")
	}
	m.inEventView = false

	// segment view
	m.inSegmentView = true
	if !m.rightFocused() {
		t.Fatal("segment view should be right-focused")
	}
	m.inSegmentView = false
}

// TestFocusBorderIndicator verifies the colored-border focus indicator:
// the focused pane's border carries the accent color (#56b6c2) and the
// unfocused pane's border carries the muted color (#5c6370), for both
// left-focus (tree) and right-focus (after tab into detail).
func TestFocusBorderIndicator(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)

	tree := agent.NewAgentTree("root", "m")
	m := NewAgentsModel(tree, nil)
	m.width, m.height = 100, 20

	// TrueColor renders hex as RGB decimal triples in the SGR sequence:
	// #56b6c2 → 86;182;194 (colCyan, accent), #5c6370 → 92;99;112 (colMuted).
	accent := "38;2;86;182;194" // colCyan, focused
	muted := "38;2;92;99;112"   // colMuted, unfocused

	// Left focus (tree): the top border row should carry both accent (tree)
	// and muted (detail) somewhere.
	view := m.View()
	if !strings.Contains(view, accent) {
		t.Errorf("left-focus view missing accent border color %s\n%s", accent, view)
	}
	if !strings.Contains(view, muted) {
		t.Errorf("left-focus view missing muted border color %s\n%s", muted, view)
	}

	// The tree pane's border top row must be accent-colored while tree is focused.
	// Find the first row and confirm accent precedes muted (tree is left, detail
	// right), i.e. accent appears in the left half of the top border line.
	top := strings.SplitN(view, "\n", 2)[0]
	ai := strings.Index(top, accent)
	mi := strings.Index(top, muted)
	if ai == -1 || mi == -1 {
		t.Fatalf("top border row missing accent or muted:\n%q", top)
	}
	if ai > mi {
		t.Errorf("left focus: accent (tree) should precede muted (detail) in top row:\n%q", top)
	}

	// Right focus (tab into detail): now detail carries accent, tree carries muted.
	m.Update(tea.KeyMsg{Type: tea.KeyTab})
	view2 := m.View()
	top2 := strings.SplitN(view2, "\n", 2)[0]
	ai2 := strings.Index(top2, accent)
	mi2 := strings.Index(top2, muted)
	if ai2 == -1 || mi2 == -1 {
		t.Fatalf("right-focus top border row missing accent or muted:\n%q", top2)
	}
	if mi2 > ai2 {
		t.Errorf("right focus: muted (tree) should precede accent (detail) in top row:\n%q", top2)
	}
}

// TestFocusWidthInvariant verifies the border does not break the exact-width
// column-join invariant: every rendered line's display width equals the full
// overlay width, and no line exceeds it.
func TestFocusWidthInvariant(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)

	tree := agent.NewAgentTree("root", "m")
	root := tree.Node(tree.RootID())
	bb := agent.NewBlackboard(tree)
	bb.Put("plan/outline", map[string]any{"steps": float64(3)}, root.ID, "root")

	for _, w := range []int{80, 100, 120} {
		m := NewAgentsModel(tree, nil).WithBlackboard(bb)
		m.width, m.height = w, 20
		// Exercise both left and right focus and the blackboard.
		for _, focus := range []func(){
			func() {},
			func() { m.Update(tea.KeyMsg{Type: tea.KeyTab}) },
			func() { m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'b'}}) },
		} {
			focus()
			view := m.View()
			for i, line := range strings.Split(view, "\n") {
				if lw := lipgloss.Width(line); lw != w {
					t.Errorf("w=%d line %d width %d != overlay width %d:\n%q", w, i, lw, w, line)
				}
			}
		}
	}
}
