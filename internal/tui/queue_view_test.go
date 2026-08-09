package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ethanhinson/fuse/internal/agent"
)

// queueEditorFixture builds a shell with three queued messages on the root node,
// then opens the /queue editor.
func queueEditorFixture(t *testing.T) (ShellModel, *agent.HumanBus, *agent.AgentTree) {
	t.Helper()
	m, bus, tree := shellWithHumanMessaging(t)
	root := tree.RootID()
	bus.Enqueue(root, agent.ModeQueued, "@root", "first message")
	bus.Enqueue(root, agent.ModeQueued, "@root", "second message")
	bus.Enqueue(root, agent.ModeQueued, "@root", "third message")
	m.running = false
	m = typeLine(m, "/queue")
	m, _ = enter(m)
	return m, bus, tree
}

func TestQueueEditor_Opens(t *testing.T) {
	m, _, _ := queueEditorFixture(t)
	if m.queue == nil {
		t.Fatal("/queue did not open the editor")
	}
	if len(m.queue.rows) != 3 {
		t.Fatalf("editor should list 3 messages, got %d", len(m.queue.rows))
	}
	view := ansiRE.ReplaceAllString(m.View(), "")
	if !strings.Contains(view, "Message queue") || !strings.Contains(view, "first message") {
		t.Errorf("queue overlay not rendered: %q", view)
	}
}

func TestQueueEditor_Delete(t *testing.T) {
	m, bus, tree := queueEditorFixture(t)
	// Cursor on the first row; delete it.
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	m = next.(ShellModel)
	pend := bus.Pending(tree.RootID())
	if len(pend) != 2 || pend[0].Text != "second message" {
		t.Fatalf("delete did not remove the first message: %+v", pend)
	}
}

func TestQueueEditor_Reorder(t *testing.T) {
	m, bus, tree := queueEditorFixture(t)
	// Move the first message down one (J).
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'J'}})
	m = next.(ShellModel)
	pend := bus.Pending(tree.RootID())
	if pend[0].Text != "second message" || pend[1].Text != "first message" {
		t.Errorf("reorder failed: %q, %q", pend[0].Text, pend[1].Text)
	}
}

func TestQueueEditor_Edit(t *testing.T) {
	m, bus, tree := queueEditorFixture(t)
	// e to edit, backspace-clear is tedious — just append text then save; the edit
	// replaces the whole buffer, which starts as the current text.
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	m = next.(ShellModel)
	if m.queue.editing == nil {
		t.Fatal("edit mode did not open")
	}
	for _, r := range " EDITED" {
		next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = next.(ShellModel)
	}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(ShellModel)
	pend := bus.Pending(tree.RootID())
	if !strings.Contains(pend[0].Text, "EDITED") {
		t.Errorf("edit not applied: %q", pend[0].Text)
	}
}

func TestQueueEditor_Close(t *testing.T) {
	m, _, _ := queueEditorFixture(t)
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = next.(ShellModel)
	if m.queue != nil {
		t.Error("Esc should close the queue editor")
	}
}
