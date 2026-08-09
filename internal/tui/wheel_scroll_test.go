package tui

import (
	"context"
	"fmt"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/ethanhinson/fuse/internal/agent"
)

func wheel(m *AgentsModel, down bool) {
	btn := tea.MouseButtonWheelUp
	if down {
		btn = tea.MouseButtonWheelDown
	}
	m.Update(tea.MouseMsg{Button: btn, Action: tea.MouseActionPress})
}

func spawnChildren(t *testing.T, tree *agent.AgentTree, n int) {
	t.Helper()
	root := tree.Node(tree.RootID())
	s := agent.NewSpawner(agent.WithTree(tree), agent.WithNode(root),
		agent.WithChildBuilder(func(context.Context, agent.SpawnOpts, *agent.AgentNode, *agent.AgentTree) (string, error) {
			return "", nil
		}))
	handles := make([]agent.AgentHandle, 0, n)
	for i := 0; i < n; i++ {
		h, err := s.Spawn(context.Background(), agent.SpawnOpts{Label: fmt.Sprintf("child-%02d", i)})
		if err != nil {
			t.Fatal(err)
		}
		handles = append(handles, h)
	}
	for _, h := range handles {
		h.Wait()
	}
}

// TestWheelTreeScrollsOffsetNotSelection: on the tree (left focus) the wheel
// moves m.treeScroll, not m.selected.
func TestWheelTreeScrollsOffsetNotSelection(t *testing.T) {
	tree := agent.NewAgentTree("root", "m")
	spawnChildren(t, tree, 30)
	m := NewAgentsModel(tree, nil)
	m.width, m.height = 100, 12
	m.refreshSnapshot()
	m.View()

	selBefore := m.selected
	scrollBefore := m.treeScroll
	for i := 0; i < 3; i++ {
		wheel(m, true) // down
		m.View()
	}
	if m.selected != selBefore {
		t.Errorf("wheel changed selection: %d -> %d (should not move)", selBefore, m.selected)
	}
	if m.treeScroll <= scrollBefore {
		t.Errorf("wheel down did not increase treeScroll: %d -> %d", scrollBefore, m.treeScroll)
	}

	// Wheel up back to top and past it: clamps at 0.
	for i := 0; i < 50; i++ {
		wheel(m, false)
	}
	if m.treeScroll < 0 {
		t.Errorf("treeScroll went negative: %d", m.treeScroll)
	}
	if m.treeScroll != 0 {
		t.Errorf("wheel up to top should clamp treeScroll at 0, got %d", m.treeScroll)
	}
}

// TestWheelDetailScrollsOffsetNotSelection: in the detail list the wheel moves
// m.detailScroll, not m.eventSel.
func TestWheelDetailScrollsOffsetNotSelection(t *testing.T) {
	tree := agent.NewAgentTree("root", "m")
	root := tree.Node(tree.RootID())
	for i := 0; i < 40; i++ {
		root.AddEvent(agent.AgentEvent{Kind: agent.KindAssistant, Payload: map[string]any{"text": "line"}})
	}
	m := NewAgentsModel(tree, nil)
	m.width, m.height = 100, 12
	m.Update(tea.KeyMsg{Type: tea.KeyTab}) // enter detail
	m.View()

	selBefore := m.eventSel
	// Wheel up from the tail: detailScroll should decrease, eventSel unchanged.
	for i := 0; i < 3; i++ {
		wheel(m, false)
		m.View()
	}
	if m.eventSel != selBefore {
		t.Errorf("wheel changed eventSel: %d -> %d (should not move)", selBefore, m.eventSel)
	}
	if m.detailScroll < 0 {
		t.Errorf("detailScroll negative: %d", m.detailScroll)
	}
}

// TestWheelBlackboardRegression is the reported bug: with the blackboard focused,
// wheel-down N times increases m.bbScroll and leaves m.selected unchanged.
func TestWheelBlackboardRegression(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)

	tree := agent.NewAgentTree("root", "m")
	root := tree.Node(tree.RootID())
	bb := agent.NewBlackboard(tree)
	for i := 0; i < 30; i++ {
		bb.Put(fmt.Sprintf("k%02d", i), map[string]any{"v": i}, root.ID, "root")
	}
	m := NewAgentsModel(tree, nil).WithBlackboard(bb)
	m.width, m.height = 100, 12
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'b'}}) // blackboard
	m.View()

	selBefore := m.selected
	bbBefore := m.bbScroll
	for i := 0; i < 4; i++ {
		wheel(m, true)
		m.View()
	}
	if m.selected != selBefore {
		t.Errorf("wheel over blackboard moved tree selection: %d -> %d", selBefore, m.selected)
	}
	if m.bbScroll <= bbBefore {
		t.Errorf("wheel down did not increase bbScroll: %d -> %d", bbBefore, m.bbScroll)
	}

	// Clamp at top.
	for i := 0; i < 50; i++ {
		wheel(m, false)
	}
	if m.bbScroll != 0 {
		t.Errorf("wheel up to top should clamp bbScroll at 0, got %d", m.bbScroll)
	}
}

// TestDetailReentryFollowsTailAfterWheel is the finding-2 regression: after the
// wheel sets detailManual on the detail list, tabbing back to the tree and
// re-entering detail must follow the tail (newest event selected, followTail
// effective) on the FIRST render — no j/k press required. Before the fix the
// stale detailManual flag suppressed selection-follow for one frame.
func TestDetailReentryFollowsTailAfterWheel(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)

	tree := agent.NewAgentTree("root", "m")
	root := tree.Node(tree.RootID())
	for i := 0; i < 40; i++ {
		root.AddEvent(agent.AgentEvent{Kind: agent.KindAssistant, Payload: map[string]any{"text": "line"}})
	}
	m := NewAgentsModel(tree, nil)
	m.width, m.height = 100, 12

	// Enter detail and wheel up to set detailManual (manual scroll off the tail).
	m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m.View()
	for i := 0; i < 5; i++ {
		wheel(m, false) // up
		m.View()
	}
	if !m.detailManual {
		t.Fatal("wheel up should have set detailManual")
	}

	// Tab back to the tree, then re-enter detail.
	m.Update(tea.KeyMsg{Type: tea.KeyTab}) // detail -> tree
	if m.detailManual {
		t.Error("exiting to the tree should clear detailManual")
	}
	if m.treeManual {
		t.Error("returning to the tree should clear treeManual")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyTab}) // tree -> detail
	if m.detailManual {
		t.Error("re-entering detail should clear detailManual")
	}

	// First render after re-entry must land on the tail without any j/k press.
	m.View()
	if !m.followTail {
		t.Error("re-entry did not follow the tail on the first render (followTail not effective)")
	}
	if m.eventSel != m.eventCount-1 {
		t.Errorf("re-entry did not select the newest event on first render: eventSel=%d eventCount=%d", m.eventSel, m.eventCount)
	}
}

// TestWheelEventViewScrollsOffset: in an expanded event the wheel moves eventScroll.
func TestWheelEventViewScrollsOffset(t *testing.T) {
	tree := agent.NewAgentTree("root", "m")
	root := tree.Node(tree.RootID())
	long := ""
	for i := 0; i < 200; i++ {
		long += "word "
	}
	root.AddEvent(agent.AgentEvent{Kind: agent.KindAssistant, Payload: map[string]any{"text": long}})
	m := NewAgentsModel(tree, nil)
	m.width, m.height = 100, 12
	m.Update(tea.KeyMsg{Type: tea.KeyTab})   // detail
	m.View()                                 // primes eventCount so enter can expand
	m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // expand event
	m.View()
	if !m.inEventView {
		t.Fatal("expected to be in event view")
	}
	before := m.eventScroll
	for i := 0; i < 3; i++ {
		wheel(m, true)
		m.View()
	}
	if m.eventScroll <= before {
		t.Errorf("wheel down did not increase eventScroll: %d -> %d", before, m.eventScroll)
	}
	for i := 0; i < 50; i++ {
		wheel(m, false)
	}
	if m.eventScroll != 0 {
		t.Errorf("wheel up to top should clamp eventScroll at 0, got %d", m.eventScroll)
	}
}
