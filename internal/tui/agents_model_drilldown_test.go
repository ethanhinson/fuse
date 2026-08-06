package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/permissions"
)

// TestEventDrilldownShowsFullContent: enter on a selected event opens its
// complete (untruncated) content; esc returns to the event list.
func TestEventDrilldownShowsFullContent(t *testing.T) {
	tree := agent.NewAgentTree("root", "m")
	root := tree.Node(tree.RootID())
	long := strings.Repeat("full-output-sentence ", 60) // ~1.2KB, would truncate in list view
	root.AddEvent(agent.AgentEvent{Kind: agent.KindToolCall, Name: "read_file", Payload: map[string]any{"args": `{"path":"x"}`}})
	root.AddEvent(agent.AgentEvent{Kind: agent.KindToolResult, Name: "read_file", Payload: map[string]any{"output": long}})

	m := NewAgentsModel(tree, nil)
	m.width, m.height = 120, 30

	// Enter detail on root, selection lands on newest event (the result).
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !m.inDetail {
		t.Fatal("should be in detail mode")
	}
	_ = m.View() // render to populate eventCount/selection
	if m.eventSel != m.eventCount-1 {
		t.Fatalf("selection should follow tail: sel=%d count=%d", m.eventSel, m.eventCount)
	}

	// Expand the event.
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !m.inEventView {
		t.Fatal("enter should open the event view")
	}
	view := m.View()
	if !strings.Contains(view, "full-output-sentence") {
		t.Error("event view should show event content")
	}
	if c := strings.Count(view, "full-output-sentence"); c < 20 {
		t.Errorf("event view should show FULL content (wrapped); found %d occurrences", c)
	}

	// Esc backs out to the event list.
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.inEventView {
		t.Error("esc should leave the event view")
	}
	if !m.inDetail {
		t.Error("esc from event view should return to detail, not tree")
	}
}

// TestTreePaneScrollsToKeepSelectionVisible: with more nodes than pane rows,
// moving the selection past the fold must window the tree, not clip it.
func TestTreePaneScrollsToKeepSelectionVisible(t *testing.T) {
	tree := agent.NewAgentTree("root", "m")
	root := tree.Node(tree.RootID())
	s := agent.NewSpawner(agent.WithTree(tree), agent.WithNode(root),
		agent.WithChildBuilder(func(_ context.Context, _ agent.SpawnOpts, _ *agent.AgentNode, _ *agent.AgentTree) (string, error) {
			return "", nil
		}))
	handles := make([]agent.AgentHandle, 0, 30)
	for i := 0; i < 30; i++ {
		h, err := s.Spawn(context.Background(), agent.SpawnOpts{Label: fmt.Sprintf("child-%02d", i)})
		if err != nil {
			t.Fatal(err)
		}
		handles = append(handles, h)
	}
	for _, h := range handles {
		h.Wait()
	}

	m := NewAgentsModel(tree, nil)
	m.width, m.height = 100, 12 // pane shows ~11 rows; 31 nodes total
	m.refreshSnapshot()

	// Jump to the last node; it must be rendered.
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'G'}})
	view := m.View()
	if !strings.Contains(view, "child-29") {
		t.Error("selected last node not visible after G")
	}
	if !strings.Contains(view, "more…") {
		t.Error("overflow indicator missing on a windowed tree")
	}

	// Back to the top; first rows visible again.
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'g'}})
	view = m.View()
	if !strings.Contains(view, "child-00") && !strings.Contains(view, "deepseek") && !strings.Contains(view, "root") {
		t.Errorf("top of tree not visible after g:\n%s", view)
	}
}

// TestTreeSummaryAppendedToTranscript: finishing a turn with subagents leaves
// a compact status tree in the chat history.
func TestTreeSummaryAppendedToTranscript(t *testing.T) {
	tree := agent.NewAgentTree("root", "alpha")
	root := tree.Node(tree.RootID())
	s := agent.NewSpawner(agent.WithTree(tree), agent.WithNode(root),
		agent.WithChildBuilder(func(_ context.Context, _ agent.SpawnOpts, _ *agent.AgentNode, _ *agent.AgentTree) (string, error) {
			return "ok", nil
		}))
	for _, label := range []string{"Explore tools", "Explore tui"} {
		h, err := s.Spawn(context.Background(), agent.SpawnOpts{Label: label})
		if err != nil {
			t.Fatal(err)
		}
		h.Wait()
	}

	m := sized(NewShellModel("alpha", false, "dark", testRegistry(), nil, nilBuilder, permissions.NewSessionMode(permissions.ModeSmart), true))
	m = m.WithTree(tree)
	m.running = true // as if a turn were in flight; runStart zero => all nodes included
	next, _ := m.Update(AgentDoneMsg{})
	m = next.(ShellModel)

	out := plainLines(m)
	if !strings.Contains(out, "2 subagent(s) this turn") {
		t.Errorf("tree summary footer missing: %q", out)
	}
	if !strings.Contains(out, "Explore tools") || !strings.Contains(out, "Explore tui") {
		t.Errorf("tree summary rows missing: %q", out)
	}
	if !strings.Contains(out, "✓") {
		t.Error("tree summary should carry status glyphs")
	}
}

// TestRootClockFreezesWhenTurnEnds: the agents view must stop counting the
// root's elapsed time once the loop finishes.
func TestRootClockFreezesWhenTurnEnds(t *testing.T) {
	tree := agent.NewAgentTree("root", "alpha")

	m := sized(NewShellModel("alpha", false, "dark", testRegistry(), nil, nilBuilder, permissions.NewSessionMode(permissions.ModeSmart), true))
	m = m.WithTree(tree)
	m = typeLine(m, "do the thing")
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	sm := next.(ShellModel)

	rootView := tree.Node(tree.RootID()).Snapshot()
	if rootView.Status != agent.StatusRunning || rootView.StartedAt.IsZero() {
		t.Fatal("BeginTurn should mark root running with a fresh clock")
	}

	next, _ = sm.Update(AgentDoneMsg{})
	_ = next.(ShellModel)
	rootView = tree.Node(tree.RootID()).Snapshot()
	if rootView.Status != agent.StatusDone {
		t.Errorf("root status after turn = %v, want done", rootView.Status)
	}
	if rootView.EndedAt.IsZero() {
		t.Error("EndTurn must freeze the root clock (EndedAt set)")
	}
	frozen := nodeElapsed(rootView)
	time.Sleep(20 * time.Millisecond)
	if nodeElapsed(rootView) != frozen {
		t.Error("elapsed must not keep counting after the turn ends")
	}
}

// TestRootClockMarksErrorTurns: a failed turn ends the root as error.
func TestRootClockMarksErrorTurns(t *testing.T) {
	tree := agent.NewAgentTree("root", "alpha")
	m := sized(NewShellModel("alpha", false, "dark", testRegistry(), nil, nilBuilder, permissions.NewSessionMode(permissions.ModeSmart), true))
	m = m.WithTree(tree)
	m = typeLine(m, "task")
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	sm := next.(ShellModel)
	next, _ = sm.Update(AgentErrMsg{Err: "boom"})
	sm = next.(ShellModel)
	next, _ = sm.Update(AgentDoneMsg{})
	_ = next.(ShellModel)
	if got := tree.Node(tree.RootID()).Snapshot().Status; got != agent.StatusError {
		t.Errorf("root status after failed turn = %v, want error", got)
	}
}
