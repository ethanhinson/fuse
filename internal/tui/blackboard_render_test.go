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

// TestBlackboardNextPrevGroupNav: with >=3 writer groups, n moves bbScroll to the
// next group's first line and p to the previous; both clamp at the ends.
func TestBlackboardNextPrevGroupNav(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)

	tree := agent.NewAgentTree("root", "m")
	bb := agent.NewBlackboard(tree)
	// Three writers, distinct WrittenAt so ordering is deterministic.
	bb.Put("c/k", "v", "id-c", "carol")
	time.Sleep(3 * time.Millisecond)
	bb.Put("b/k", "v", "id-b", "bob")
	time.Sleep(3 * time.Millisecond)
	bb.Put("a/k", "v", "id-a", "alice")

	m := NewAgentsModel(tree, nil).WithBlackboard(bb)
	m.width, m.height = 100, 30
	enterBoard(m)
	m.buildBlackboardLines(60) // prime

	// Groups most-recent-first: alice(0), bob(?), carol(?). Compute starts.
	_, starts := m.blackboardBody(bb.Snapshot(), 60)
	if len(starts) < 3 {
		t.Fatalf("expected >=3 group starts, got %d", len(starts))
	}

	// From top, n moves to second group's start.
	m.bbScroll = 0
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	if m.bbScroll != starts[1] {
		t.Errorf("n from group 0 should land on starts[1]=%d, got %d", starts[1], m.bbScroll)
	}
	// n again -> third group.
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	if m.bbScroll != starts[2] {
		t.Errorf("n should land on starts[2]=%d, got %d", starts[2], m.bbScroll)
	}
	// n at last group clamps (stays at last).
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	if m.bbScroll != starts[2] {
		t.Errorf("n at last group should clamp at starts[2]=%d, got %d", starts[2], m.bbScroll)
	}

	// p walks back.
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}})
	if m.bbScroll != starts[1] {
		t.Errorf("p should land on starts[1]=%d, got %d", starts[1], m.bbScroll)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}})
	if m.bbScroll != starts[0] {
		t.Errorf("p should land on starts[0]=%d, got %d", starts[0], m.bbScroll)
	}
	// p at first group clamps at first.
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}})
	if m.bbScroll != starts[0] {
		t.Errorf("p at first group should clamp at starts[0]=%d, got %d", starts[0], m.bbScroll)
	}
}

// ─── change 0066: turn awareness inside a writer group ────────────────────────

// bbTurnFixture builds a two-turn root plus a hand-stamped snapshot so entry
// timestamps sit deterministically on either side of the turn-2 boundary.
func bbTurnFixture(t *testing.T) (*AgentsModel, map[string]agent.BlackboardEntry, agent.NodeView) {
	t.Helper()
	tree := agent.NewAgentTree("root", "m")
	tree.BeginTurnWithPrompt("first")
	time.Sleep(30 * time.Millisecond)
	tree.BeginTurnWithPrompt("second")

	root := tree.Node(tree.RootID()).Snapshot()
	if len(root.Turns) != 2 {
		t.Fatalf("fixture wants 2 turn marks, got %d", len(root.Turns))
	}
	t1, t2 := root.Turns[0].StartedAt, root.Turns[1].StartedAt
	snap := map[string]agent.BlackboardEntry{
		// turn 1 (both before the turn-2 boundary)
		"alice/early": {Value: "e", WriterID: "id-a", WriterLabel: "alice", WrittenAt: t1.Add(-5 * time.Second)},
		"alice/one":   {Value: "1", WriterID: "id-a", WriterLabel: "alice", WrittenAt: t1.Add(time.Millisecond)},
		// turn 2
		"alice/two": {Value: "2", WriterID: "id-a", WriterLabel: "alice", WrittenAt: t2.Add(12300 * time.Millisecond)},
	}
	m := NewAgentsModel(tree, nil)
	m.width, m.height = 100, 40
	return m, snap, root
}

// TestBlackboardTurnDividerSplitsGroup: a writer group spanning two turns gets a
// "── turn N ──" sub-divider before each bucket, entries on the correct side.
func TestBlackboardTurnDividerSplitsGroup(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)

	m, snap, _ := bbTurnFixture(t)
	body, starts := m.blackboardBody(snap, 60)
	lines := stripLines(body)

	d1 := indexOfLineContaining(lines, "── turn 1 ──")
	d2 := indexOfLineContaining(lines, "── turn 2 ──")
	if d1 < 0 || d2 < 0 {
		t.Fatalf("want both turn dividers, got d1=%d d2=%d in:\n%s", d1, d2, strings.Join(lines, "\n"))
	}
	if d1 >= d2 {
		t.Errorf("turn buckets out of ascending order: turn1 at %d, turn2 at %d", d1, d2)
	}
	// The single writer group's header still precedes everything.
	if len(starts) != 1 || starts[0] != 0 {
		t.Fatalf("writer-group starts = %v, want [0]", starts)
	}
	if h := indexOfLineContaining(lines, "▌ alice"); h != 0 || h > d1 {
		t.Errorf("writer header at %d, want line 0 before the first divider (%d)", h, d1)
	}
	// Entries land on the correct side of the boundary.
	for _, tc := range []struct {
		key                   string
		wantAfter, wantBefore int
	}{
		{"alice/early", d1, d2},
		{"alice/one", d1, d2},
	} {
		i := indexOfLineContaining(lines, tc.key)
		if i < tc.wantAfter || i > tc.wantBefore {
			t.Errorf("%s at line %d, want inside the turn-1 bucket (%d,%d)", tc.key, i, tc.wantAfter, tc.wantBefore)
		}
	}
	if i := indexOfLineContaining(lines, "alice/two"); i < d2 {
		t.Errorf("alice/two at line %d, want after the turn-2 divider at %d", i, d2)
	}
}

// TestBlackboardEntryOffsetsAreTurnRelative: per-entry offsets are non-negative
// and measured from the entry's OWN turn start, not the session start.
func TestBlackboardEntryOffsetsAreTurnRelative(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)

	m, snap, root := bbTurnFixture(t)
	lines := stripLines(mustBody(m, snap, 60))

	for _, l := range lines {
		if strings.Contains(l, "+-") || strings.Contains(l, "· -") {
			t.Errorf("negative offset rendered: %q", l)
		}
	}
	// alice/two was written 12.3s into turn 2 — measured against the session start
	// it would be far larger, so this pins turn-relative attribution.
	want := "+" + eventOffset(root, snap["alice/two"].WrittenAt)
	if want != "+012.3s" {
		t.Fatalf("fixture offset drifted: %q", want)
	}
	i := indexOfLineContaining(lines, "alice/two")
	if i < 0 || !strings.Contains(lines[i+1], want) {
		t.Errorf("alice/two meta = %q, want offset %q", lines[i+1], want)
	}
	// The pre-turn-1 entry clamps to zero rather than going negative.
	j := indexOfLineContaining(lines, "alice/early")
	if !strings.Contains(lines[j+1], "+000.0s") {
		t.Errorf("pre-turn-1 entry meta = %q, want clamped +000.0s", lines[j+1])
	}
}

func mustBody(m *AgentsModel, snap map[string]agent.BlackboardEntry, w int) []string {
	body, _ := m.blackboardBody(snap, w)
	return body
}

// TestBlackboardSingleTurnRendersUnchanged is the backward-compatibility guard:
// with zero or one turn mark the body is byte-identical to the pre-0066 render —
// no dividers, no offsets in the meta line.
func TestBlackboardSingleTurnRendersUnchanged(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)

	_, snap, _ := bbTurnFixture(t)

	noTurn := agent.NewAgentTree("root", "m")
	mNo := NewAgentsModel(noTurn, nil)
	mNo.width, mNo.height = 100, 40

	oneTurn := agent.NewAgentTree("root", "m")
	oneTurn.BeginTurnWithPrompt("only")
	mOne := NewAgentsModel(oneTurn, nil)
	mOne.width, mOne.height = 100, 40

	legacy, ls := mNo.blackboardBody(snap, 60)
	single, ss := mOne.blackboardBody(snap, 60)
	if strings.Join(legacy, "\n") != strings.Join(single, "\n") {
		t.Errorf("single-turn body diverged from the no-turn (legacy) body")
	}
	if fmt.Sprint(ls) != fmt.Sprint(ss) {
		t.Errorf("group starts diverged: %v vs %v", ls, ss)
	}
	for _, l := range stripLines(legacy) {
		if strings.Contains(l, "── turn ") {
			t.Errorf("legacy path emitted a turn divider: %q", l)
		}
		if strings.Contains(l, "written by") && strings.Contains(l, "+0") {
			t.Errorf("legacy path emitted a turn offset: %q", l)
		}
	}
}

// TestBlackboardRowsFitWidth: no rendered row exceeds the pane width, and the new
// divider rows are exactly w cells, across several widths.
func TestBlackboardRowsFitWidth(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)

	m, snap, _ := bbTurnFixture(t)
	for _, w := range []int{80, 100, 120} {
		for _, l := range stripLines(mustBody(m, snap, w)) {
			if got := lipgloss.Width(l); got > w {
				t.Errorf("w=%d: row %q is %d cells", w, l, got)
			}
			if strings.Contains(l, "── turn ") && lipgloss.Width(l) != w {
				t.Errorf("w=%d: divider %q is %d cells, want exactly w", w, l, lipgloss.Width(l))
			}
		}
	}
}
