package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/ethanhinson/fuse/internal/agent"
)

// TestBlackboardTabScrollClampsAtEnd refutes the "bbScroll has no upper bound"
// concern: holding 'j' (scroll down) past the content must clamp — the board
// keeps at least the last row visible and never renders a fully empty pane or
// panics. buildBlackboardLines clamps bbScroll to max(0, len(body)-rows) each
// render (spec Decision 2).
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

// TestBlackboardBottomClampToLastFullWindow asserts the spec Decision 2 bottom
// clamp: with content taller than the pane, slamming scroll-down far past the end
// must leave bbScroll at max(0, len(body)-visibleRows) — the last FULL window,
// not a near-empty pane clamped to len(body)-1.
func TestBlackboardBottomClampToLastFullWindow(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)

	tree := agent.NewAgentTree("root", "m")
	root := tree.Node(tree.RootID())
	bb := agent.NewBlackboard(tree)
	for i := 0; i < 20; i++ {
		bb.Put(fmt.Sprintf("k%02d", i), map[string]any{"v": i}, root.ID, "root")
	}

	m := NewAgentsModel(tree, nil).WithBlackboard(bb)
	m.width, m.height = 100, 12 // small height so content exceeds the window

	// Compute the render's own body length and body-row budget the same way View()
	// does, so the expected bottom clamp is exact. View() temporarily shrinks
	// m.height (scheduler-header + border rows) before buildBlackboardLines reads
	// it, and buildBlackboardLines then spends 3 of those rows on header/rule/help
	// (rows = m.height-3). Mirror that here rather than assuming the raw height.
	body, _ := m.blackboardBody(bb.Snapshot(), m.blackboardContentWidth())
	header := m.schedulerHeaderLines(m.width)
	if len(header) > m.height-1 {
		header = header[:m.height-1]
	}
	h := m.height - len(header)
	if h < 1 {
		h = 1
	}
	contentH := h - 2
	if contentH < 1 {
		contentH = 1
	}
	visibleRows := contentH - 3 // the "rows" budget buildBlackboardLines computes
	if visibleRows < 1 {
		visibleRows = 1
	}
	wantMax := len(body) - visibleRows
	if wantMax < 0 {
		wantMax = 0
	}
	if len(body) <= visibleRows {
		t.Fatalf("test needs content taller than the pane: len(body)=%d visibleRows=%d", len(body), visibleRows)
	}

	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'b'}})
	// Slam 'j' far past the end.
	for i := 0; i < 500; i++ {
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	}
	m.View() // render applies the clamp

	// The clamp must land on the last FULL window (max(0, len(body)-visibleRows)),
	// not len(body)-1 (which would leave a near-empty pane).
	if m.bbScroll != wantMax {
		t.Errorf("bottom clamp = %d, want max(0, len(body)-visibleRows) = %d (len(body)=%d visibleRows=%d)",
			m.bbScroll, wantMax, len(body), visibleRows)
	}
	if m.bbScroll == len(body)-1 {
		t.Errorf("bottom clamp regressed to len(body)-1 (%d): over-scrolls into a near-empty pane", m.bbScroll)
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

// TestBlackboardGroupStartsExactWithTurnDividers is the scroll-correctness guard
// for change 0066: turn sub-dividers add lines INSIDE a writer group, so n/p is
// only exact if blackboardGroupStarts() is derived from the same render that
// emits them. It asserts the returned starts are exactly the writer-header line
// indices of the rendered body, and that dividers really do sit between them
// (so a divider-blind line count would be wrong, not accidentally right).
func TestBlackboardGroupStartsExactWithTurnDividers(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)

	tree := agent.NewAgentTree("root", "m")
	bb := agent.NewBlackboard(tree)
	tree.BeginTurnWithPrompt("first")
	bb.Put("alice/a", "1", "id-a", "alice")
	bb.Put("bob/a", "1", "id-b", "bob")
	time.Sleep(20 * time.Millisecond)
	tree.BeginTurnWithPrompt("second")
	bb.Put("alice/b", "2", "id-a", "alice")
	bb.Put("bob/b", "2", "id-b", "bob")

	m := NewAgentsModel(tree, nil).WithBlackboard(bb)
	m.width, m.height = 100, 40
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'b'}})

	w := m.blackboardContentWidth()
	body, _ := m.blackboardBody(bb.Snapshot(), w)
	lines := stripLines(body)

	var wantStarts, dividers []int
	for i, l := range lines {
		switch {
		case strings.HasPrefix(strings.TrimSpace(l), "▌"):
			wantStarts = append(wantStarts, i)
		case strings.Contains(l, "── turn "):
			dividers = append(dividers, i)
		}
	}
	if len(wantStarts) != 2 {
		t.Fatalf("want 2 writer groups, got %d:\n%s", len(wantStarts), strings.Join(lines, "\n"))
	}
	if len(dividers) == 0 {
		t.Fatalf("fixture produced no turn dividers; the guard would be vacuous")
	}
	// At least one divider must precede the second group's start, so a
	// divider-blind numbering could not produce the same answer.
	if dividers[0] >= wantStarts[1] {
		t.Fatalf("no divider inside the first group (dividers=%v, starts=%v)", dividers, wantStarts)
	}
	got := m.blackboardGroupStarts()
	if fmt.Sprint(got) != fmt.Sprint(wantStarts) {
		t.Errorf("blackboardGroupStarts() = %v, want writer-header lines %v", got, wantStarts)
	}
	// And n/p actually lands on a writer header, dividers notwithstanding.
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	if m.bbScroll != wantStarts[1] {
		t.Errorf("after 'n' bbScroll = %d, want the second writer header at %d", m.bbScroll, wantStarts[1])
	}
	if !strings.HasPrefix(strings.TrimSpace(lines[m.bbScroll]), "▌") {
		t.Errorf("'n' landed on %q, not a writer header", lines[m.bbScroll])
	}
}
