package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

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
