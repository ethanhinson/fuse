package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/ethanhinson/fuse/internal/agent"
)

// bbLine indices in buildBlackboardLines output: [0]=header "Blackboard",
// [1]=rule, body starts at [2].

// enterBoard opens the blackboard section and returns the model.
func enterBoard(m *AgentsModel) {
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'b'}})
}

// stripLines strips ANSI styling from each line so substring/order assertions
// read the plain text.
func stripLines(lines []string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = string(stripANSI([]byte(l)))
	}
	return out
}

// indexOfLineContaining returns the index of the first line containing sub, or -1.
func indexOfLineContaining(lines []string, sub string) int {
	for i, l := range lines {
		if strings.Contains(l, sub) {
			return i
		}
	}
	return -1
}

// TestBlackboardGroupedByWriter: entries bucket under a per-writer header, most
// recent writer group first, keys sorted alphabetically within a group.
func TestBlackboardGroupedByWriter(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)

	tree := agent.NewAgentTree("root", "m")
	bb := agent.NewBlackboard(tree)
	// alice writes first (older), bob writes later (newer). Most-recent-first
	// ordering => bob's group precedes alice's.
	bb.Put("alice/z", "1", "id-a", "alice")
	bb.Put("alice/a", "2", "id-a", "alice")
	time.Sleep(5 * time.Millisecond)
	bb.Put("bob/b", "3", "id-b", "bob")

	m := NewAgentsModel(tree, nil).WithBlackboard(bb)
	m.width, m.height = 100, 40
	enterBoard(m)

	lines := stripLines(m.buildBlackboardLines(60))
	joined := strings.Join(lines, "\n")

	if !strings.Contains(joined, "alice") {
		t.Errorf("missing alice group header:\n%s", joined)
	}
	if !strings.Contains(joined, "bob") {
		t.Errorf("missing bob group header:\n%s", joined)
	}

	iBob := indexOfLineContaining(lines, "bob")
	iAlice := indexOfLineContaining(lines, "alice")
	if iBob == -1 || iAlice == -1 {
		t.Fatalf("group headers not found: bob=%d alice=%d", iBob, iAlice)
	}
	if iBob > iAlice {
		t.Errorf("most-recent-first: bob group (%d) should precede alice group (%d)", iBob, iAlice)
	}

	// Keys sorted within alice's group: alice/a before alice/z.
	iAa := indexOfLineContaining(lines, "alice/a")
	iAz := indexOfLineContaining(lines, "alice/z")
	if iAa == -1 || iAz == -1 {
		t.Fatalf("alice keys not found: a=%d z=%d\n%s", iAa, iAz, joined)
	}
	if iAa > iAz {
		t.Errorf("keys not sorted in group: alice/a (%d) should precede alice/z (%d)", iAa, iAz)
	}
}

// TestBlackboardUnknownWriter: an entry with empty label and id groups under
// "(unknown)".
func TestBlackboardUnknownWriter(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)

	tree := agent.NewAgentTree("root", "m")
	bb := agent.NewBlackboard(tree)
	bb.Put("orphan/key", "v", "", "") // no id, no label

	m := NewAgentsModel(tree, nil).WithBlackboard(bb)
	m.width, m.height = 100, 40
	enterBoard(m)

	joined := strings.Join(stripLines(m.buildBlackboardLines(60)), "\n")
	if !strings.Contains(joined, "(unknown)") {
		t.Errorf("empty-writer entry should group under (unknown):\n%s", joined)
	}
}

// TestBlackboardWriterIDFallback: an entry with empty label but a writer id
// groups under the id.
func TestBlackboardWriterIDFallback(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)

	tree := agent.NewAgentTree("root", "m")
	bb := agent.NewBlackboard(tree)
	bb.Put("k", "v", "worker-7", "") // id, no label

	m := NewAgentsModel(tree, nil).WithBlackboard(bb)
	m.width, m.height = 100, 40
	enterBoard(m)

	joined := strings.Join(stripLines(m.buildBlackboardLines(60)), "\n")
	if !strings.Contains(joined, "worker-7") {
		t.Errorf("empty-label entry should fall back to writer id:\n%s", joined)
	}
}

// TestBlackboardStickyHeader: with a small window, scrolling past the first
// group's header keeps the current group's header pinned at the top.
func TestBlackboardStickyHeader(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)

	tree := agent.NewAgentTree("root", "m")
	bb := agent.NewBlackboard(tree)
	// One writer, many keys so the group's body scrolls past its own header.
	for i := 0; i < 20; i++ {
		bb.Put("alice/"+string(rune('a'+i)), "v", "id-a", "alice")
	}

	m := NewAgentsModel(tree, nil).WithBlackboard(bb)
	m.width, m.height = 100, 12
	enterBoard(m)

	// Scroll well past the first group's header line.
	m.bbScroll = 8
	lines := stripLines(m.buildBlackboardLines(60))
	// Body window starts at line index 2 (after header + rule). The first body
	// line must be the pinned "alice" header even though we scrolled past it.
	if len(lines) < 3 {
		t.Fatalf("too few lines: %d", len(lines))
	}
	if !strings.Contains(lines[2], "alice") {
		t.Errorf("sticky header not pinned at top of scrolled body; line[2]=%q\nall:\n%s",
			lines[2], strings.Join(lines, "\n"))
	}
}

// TestBlackboardContrastAndSeparators: value lines carry colNormal (#abb2bf),
// key lines carry colCyan (#56b6c2), meta labels carry colMuted (#5c6370), and a
// separator appears between entries within a group.
func TestBlackboardContrastAndSeparators(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)

	tree := agent.NewAgentTree("root", "m")
	bb := agent.NewBlackboard(tree)
	bb.Put("alice/a", "value-one", "id-a", "alice")
	bb.Put("alice/b", "value-two", "id-a", "alice")

	m := NewAgentsModel(tree, nil).WithBlackboard(bb)
	m.width, m.height = 100, 40
	enterBoard(m)

	lines := m.buildBlackboardLines(60) // styled (not stripped)

	normal := "38;2;171;178;191" // colNormal
	cyan := "38;2;86;182;194"    // colCyan
	muted := "38;2;92;99;112"    // colMuted

	// Find the value line ("value-one") and assert it's colNormal.
	valueStyled := ""
	keyStyled := ""
	metaStyled := ""
	for _, l := range lines {
		plain := string(stripANSI([]byte(l)))
		if strings.Contains(plain, "value-one") {
			valueStyled = l
		}
		if strings.Contains(plain, "alice/a") {
			keyStyled = l
		}
		if strings.Contains(plain, "written by") {
			metaStyled = l
		}
	}
	if valueStyled == "" || !strings.Contains(valueStyled, normal) {
		t.Errorf("value line should carry colNormal %s: %q", normal, valueStyled)
	}
	if keyStyled == "" || !strings.Contains(keyStyled, cyan) {
		t.Errorf("key line should carry colCyan %s: %q", cyan, keyStyled)
	}
	if metaStyled == "" || !strings.Contains(metaStyled, muted) {
		t.Errorf("meta line should carry colMuted %s: %q", muted, metaStyled)
	}

	// A separator (muted ─ run) appears between the two entries in the group.
	plainLines := stripLines(lines)
	iA := indexOfLineContaining(plainLines, "value-one")
	iB := indexOfLineContaining(plainLines, "value-two")
	if iA == -1 || iB == -1 || iB <= iA {
		t.Fatalf("both entries expected in order: a=%d b=%d", iA, iB)
	}
	sep := false
	for _, l := range plainLines[iA:iB] {
		if strings.Contains(l, "───") {
			sep = true
		}
	}
	if !sep {
		t.Errorf("expected a separator between entries within the group:\n%s", strings.Join(plainLines[iA:iB], "\n"))
	}
}

// TestBlackboardPrettyJSON: a nested object renders as multi-line indented JSON
// (nested key on its own indented line); a scalar renders inline; every produced
// line fits a narrow pane width.
func TestBlackboardPrettyJSON(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)

	tree := agent.NewAgentTree("root", "m")
	bb := agent.NewBlackboard(tree)
	bb.Put("nested/obj", map[string]any{"a": map[string]any{"b": float64(1)}}, "id", "w")
	bb.Put("scalar/s", "hello", "id", "w")

	m := NewAgentsModel(tree, nil).WithBlackboard(bb)
	narrow := 30
	m.width, m.height = 100, 40
	enterBoard(m)

	lines := stripLines(m.buildBlackboardLines(narrow))
	joined := strings.Join(lines, "\n")

	// Nested key "b" on its own indented line (2-space or deeper indentation).
	foundNestedIndent := false
	for _, l := range lines {
		if strings.Contains(l, `"b"`) && strings.HasPrefix(l, "  ") {
			foundNestedIndent = true
		}
	}
	if !foundNestedIndent {
		t.Errorf("nested object should render multi-line indented JSON with \"b\" on its own indented line:\n%s", joined)
	}

	// Scalar renders inline next to its key: the value string appears.
	if !strings.Contains(joined, "hello") {
		t.Errorf("scalar value should render:\n%s", joined)
	}

	// Every produced line fits the narrow width.
	for i, l := range stripLines(m.buildBlackboardLines(narrow)) {
		if lipgloss.Width(l) > narrow {
			t.Errorf("line %d exceeds narrow width %d: %q (w=%d)", i, narrow, l, lipgloss.Width(l))
		}
	}
}
