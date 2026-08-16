package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/ethanhinson/fuse/internal/agent"
)

// threeTurnRoot builds a root with three conversational turns: turns 1 and 2
// are settled, turn 3 is in flight. Event counts per turn are 2, 3, 2.
func threeTurnRoot() (agent.NodeView, []agent.AgentEvent) {
	t1 := turnsBase
	t2 := turnsBase.Add(10 * time.Minute)
	t3 := turnsBase.Add(20 * time.Minute)
	n := agent.NodeView{
		ID:        "root",
		Label:     "root",
		Status:    agent.StatusRunning,
		StartedAt: t1,
		Turns: []agent.TurnMark{
			{Turn: 1, StartedAt: t1, EndedAt: t1.Add(30 * time.Second), Prompt: "list the files"},
			{Turn: 2, StartedAt: t2, EndedAt: t2.Add(75 * time.Second), Prompt: "now summarize them"},
			{Turn: 3, StartedAt: t3, Prompt: "and write the report"},
		},
	}
	events := []agent.AgentEvent{
		evtAt(agent.KindAssistant, "a1", t1.Add(time.Second)),
		evtAt(agent.KindToolCall, "ls", t1.Add(2*time.Second)),

		evtAt(agent.KindAssistant, "a2", t2.Add(time.Second)),
		evtAt(agent.KindToolCall, "read", t2.Add(2*time.Second)),
		evtAt(agent.KindToolResult, "read", t2.Add(3*time.Second)),

		evtAt(agent.KindAssistant, "a3", t3.Add(time.Second)),
		evtAt(agent.KindToolCall, "write", t3.Add(2*time.Second)),
	}
	return n, events
}

// turnEventCounts is the per-turn event tally of threeTurnRoot.
var turnEventCounts = map[int]int{1: 2, 2: 3, 3: 2}

func headerRows(rows []detailRow) []detailRow {
	var out []detailRow
	for _, r := range rows {
		if r.header {
			out = append(out, r)
		}
	}
	return out
}

func eventRowsForTurn(rows []detailRow, turn int) []detailRow {
	var out []detailRow
	for _, r := range rows {
		if !r.header && r.turn == turn {
			out = append(out, r)
		}
	}
	return out
}

func TestDetailRowsEmitsOneHeaderPerTurnInOrder(t *testing.T) {
	n, events := threeTurnRoot()
	m := &AgentsModel{}

	rows := m.detailRows(n, events)
	hdrs := headerRows(rows)
	if len(hdrs) != 3 {
		t.Fatalf("header rows = %d, want 3 (one per turn); rows=%+v", len(hdrs), rows)
	}
	for i, h := range hdrs {
		if h.turn != i+1 {
			t.Errorf("header %d is turn %d, want %d (turn order)", i, h.turn, i+1)
		}
		if h.evtIdx != -1 {
			t.Errorf("header %d evtIdx = %d, want -1", i, h.evtIdx)
		}
	}
}

func TestDetailRowsDefaultCollapsesAllButLastTurn(t *testing.T) {
	n, events := threeTurnRoot()
	m := &AgentsModel{}

	rows := m.detailRows(n, events)
	for _, turn := range []int{1, 2} {
		if got := eventRowsForTurn(rows, turn); len(got) != 0 {
			t.Errorf("turn %d rendered %d event rows, want 0 (collapsed by default)", turn, len(got))
		}
	}
	if got, want := len(eventRowsForTurn(rows, 3)), turnEventCounts[3]; got != want {
		t.Errorf("turn 3 rendered %d event rows, want %d (the current turn is expanded)", got, want)
	}
	// 3 headers + turn 3's events, nothing else.
	if got, want := len(rows), 3+turnEventCounts[3]; got != want {
		t.Errorf("total rows = %d, want %d", got, want)
	}
}

func TestDetailRowsEventIndicesAttributeByTimestamp(t *testing.T) {
	n, events := threeTurnRoot()
	m := &AgentsModel{turnExpanded: map[int]bool{1: true, 2: true, 3: true}}

	rows := m.detailRows(n, events)
	if got, want := len(rows), 3+len(events); got != want {
		t.Fatalf("fully expanded rows = %d, want %d", got, want)
	}
	for turn, want := range turnEventCounts {
		if got := len(eventRowsForTurn(rows, turn)); got != want {
			t.Errorf("turn %d has %d event rows, want %d", turn, got, want)
		}
	}
	// Every event appears exactly once, and each event row's index resolves to
	// an event whose timestamp really is inside its turn.
	seen := map[int]bool{}
	for _, r := range rows {
		if r.header {
			continue
		}
		if r.evtIdx < 0 || r.evtIdx >= len(events) {
			t.Fatalf("event row index %d out of range", r.evtIdx)
		}
		if seen[r.evtIdx] {
			t.Fatalf("event %d rendered twice", r.evtIdx)
		}
		seen[r.evtIdx] = true
		if got := turnStartFor(n, events[r.evtIdx].TS); !got.Equal(n.Turns[r.turn-1].StartedAt) {
			t.Errorf("event %d placed under turn %d, but turnStartFor says %v", r.evtIdx, r.turn, got)
		}
	}
	if len(seen) != len(events) {
		t.Errorf("%d of %d events rendered", len(seen), len(events))
	}
}

// TestDetailRowsEventsBeforeTurnOneRenderUnderTurnOne — the pre-first-prompt
// bucket has no header of its own; it joins turn 1 (matching turnStartFor).
func TestDetailRowsEventsBeforeTurnOneRenderUnderTurnOne(t *testing.T) {
	n, events := threeTurnRoot()
	early := evtAt(agent.KindAssistant, "early", n.Turns[0].StartedAt.Add(-30*time.Second))
	events = append([]agent.AgentEvent{early}, events...)

	m := &AgentsModel{turnExpanded: map[int]bool{1: true}}
	rows := m.detailRows(n, events)
	if len(headerRows(rows)) != 3 {
		t.Fatalf("header rows = %d, want 3 (no extra header for the pre-turn-1 bucket)", len(headerRows(rows)))
	}
	if got, want := len(eventRowsForTurn(rows, 1)), turnEventCounts[1]+1; got != want {
		t.Errorf("turn 1 has %d event rows, want %d (the pre-turn-1 event joins it)", got, want)
	}
	if rows[0].turn != 1 || !rows[0].header {
		t.Errorf("first row = %+v, want turn 1's header", rows[0])
	}
	if rows[1].evtIdx != 0 {
		t.Errorf("first event row under turn 1 = %d, want the pre-turn-1 event (0)", rows[1].evtIdx)
	}
}

// --- guard: single-turn and child nodes take the legacy path ---------------

func TestDetailRowsSingleTurnRootIsLegacyOneRowPerEvent(t *testing.T) {
	n, events := threeTurnRoot()
	n.Turns = n.Turns[:1]
	m := &AgentsModel{}

	rows := m.detailRows(n, events)
	if len(rows) != len(events) {
		t.Fatalf("single-turn rows = %d, want %d (one per event, no headers)", len(rows), len(events))
	}
	for i, r := range rows {
		if r.header || r.evtIdx != i {
			t.Fatalf("row %d = %+v, want the legacy non-header row for event %d", i, r, i)
		}
	}
}

func TestDetailRowsChildNodeNoTurnsIsLegacy(t *testing.T) {
	_, events := threeTurnRoot()
	n := agent.NodeView{ID: "child", StartedAt: turnsBase}
	m := &AgentsModel{}

	rows := m.detailRows(n, events)
	if len(rows) != len(events) {
		t.Fatalf("child rows = %d, want %d", len(rows), len(events))
	}
	for i, r := range rows {
		if r.header || r.evtIdx != i {
			t.Fatalf("row %d = %+v, want the legacy row for event %d", i, r, i)
		}
	}
}

// legacyEventLinesGolden is the pre-0066 detail renderer, verbatim: one
// selectable row per (pre-filtered) event, the row at m.eventSel highlighted.
// It is NOT production code — renderDetailRows replaced it on every path — and
// it lives here precisely so that stays true. It is the golden reference the
// backward-compatibility guard below pins renderDetailRows' single-turn/child
// output against; deleting it would delete the pin.
func (m *AgentsModel) legacyEventLinesGolden(n agent.NodeView, events []agent.AgentEvent, w int) []string {
	lines := make([]string, 0, len(events))
	for i, evt := range events {
		lines = append(lines, m.renderEventRow(n, evt, i == m.eventSel, w))
	}
	return lines
}

// TestRenderDetailRowsLegacyByteIdenticalToEventLines is the backward-compat
// guard: for a single-turn root and for a child node, the row-model renderer
// must emit exactly what the pre-change event renderer emits.
func TestRenderDetailRowsLegacyByteIdenticalToEventLines(t *testing.T) {
	_, events := threeTurnRoot()
	single, _ := threeTurnRoot()
	single.Turns = single.Turns[:1]
	child := agent.NodeView{ID: "child", Label: "child", StartedAt: turnsBase}

	for name, n := range map[string]agent.NodeView{"single-turn root": single, "child": child} {
		for _, sel := range []int{0, 3, len(events) - 1} {
			m := &AgentsModel{eventSel: sel, rowSel: sel}
			want := m.legacyEventLinesGolden(n, events, 100)
			got := m.renderDetailRows(n, events, m.detailRows(n, events), 100)
			if len(got) != len(want) {
				t.Fatalf("%s sel=%d: %d rows, want %d", name, sel, len(got), len(want))
			}
			for i := range want {
				if got[i] != want[i] {
					t.Errorf("%s sel=%d row %d not byte-identical:\n got %q\nwant %q", name, sel, i, got[i], want[i])
				}
			}
		}
	}
}

// --- header rendering -----------------------------------------------------

func TestTurnHeaderShowsOrdinalPromptAndDuration(t *testing.T) {
	n, events := threeTurnRoot()
	m := &AgentsModel{}
	lines := m.renderDetailRows(n, events, m.detailRows(n, events), 120)

	rows := m.detailRows(n, events)
	var hdrLines []string
	for i, r := range rows {
		if r.header {
			hdrLines = append(hdrLines, stripANSITurns(lines[i]))
		}
	}
	if len(hdrLines) != 3 {
		t.Fatalf("got %d header lines, want 3", len(hdrLines))
	}
	for i, want := range []string{"turn 1", "turn 2", "turn 3"} {
		if !strings.Contains(hdrLines[i], want) {
			t.Errorf("header %d = %q, want it to contain %q", i, hdrLines[i], want)
		}
	}
	if !strings.Contains(hdrLines[0], "list the files") {
		t.Errorf("turn 1 header %q lacks its prompt preview", hdrLines[0])
	}
	// Settled turns show their duration; the in-flight one reads "running".
	if !strings.Contains(hdrLines[0], "30s") {
		t.Errorf("turn 1 header %q lacks its 30s duration", hdrLines[0])
	}
	if !strings.Contains(hdrLines[1], "1m15s") {
		t.Errorf("turn 2 header %q lacks its 1m15s duration", hdrLines[1])
	}
	if !strings.Contains(hdrLines[2], "running") {
		t.Errorf("turn 3 header %q should read running (EndedAt zero)", hdrLines[2])
	}
	// Collapsed headers carry an event count and the collapsed glyph; the
	// expanded one does not.
	if !strings.Contains(hdrLines[0], "▸") || !strings.Contains(hdrLines[0], "2 events") {
		t.Errorf("collapsed turn 1 header %q wants ▸ and its event count", hdrLines[0])
	}
	if !strings.Contains(hdrLines[1], "3 events") {
		t.Errorf("collapsed turn 2 header %q wants its event count", hdrLines[1])
	}
	if !strings.Contains(hdrLines[2], "▾") || strings.Contains(hdrLines[2], "events") {
		t.Errorf("expanded turn 3 header %q wants ▾ and no event count", hdrLines[2])
	}
}

func TestTurnHeaderEmptyPromptReadsNoPrompt(t *testing.T) {
	n, events := threeTurnRoot()
	n.Turns[0].Prompt = ""
	m := &AgentsModel{}
	rows := m.detailRows(n, events)
	lines := m.renderDetailRows(n, events, rows, 100)
	if got := stripANSITurns(lines[0]); !strings.Contains(got, "(no prompt)") {
		t.Errorf("header with empty prompt = %q, want it to read (no prompt)", got)
	}
}

// TestTurnHeaderSanitizesHostilePromptBytes: the prompt is operator/model-
// adjacent text (learning sanitize-untrusted-bytes-fixed-width-tui). ESC, CR
// and tab bytes must not reach the terminal, and a huge prompt must not
// overflow the row.
func TestTurnHeaderSanitizesHostilePromptBytes(t *testing.T) {
	hostile := "\x1b[31mred\x1b[0m\ttab\rcr\x01\x7f\nsecond line " + strings.Repeat("x", 400)

	prompts := []struct{ name, prompt string }{
		// The original fixture: control bytes plus enough filler to overflow.
		{"hostile", hostile},
		// Pure ASCII. The hostile fixture's leading ESC bytes become multi-byte
		// "·" runes, so a BYTE-budgeted truncation cuts far short of the CELL
		// budget and hides an off-by-one; ASCII exposes it exactly.
		{"ascii", strings.Repeat("a", 400)},
		// Wide runes: a cell-aware truncation must FILL the budget here rather
		// than under-using two thirds of it, and still never exceed it.
		{"cjk", strings.Repeat("漢字", 200)},
	}

	// The preview itself must be free of every terminal-altering byte, and must
	// respect its budget — quotes AND ellipsis included. Asserted on the unstyled
	// helper so the assertion cannot be masked by the row's own SGR sequences.
	for _, p := range prompts {
		for _, budget := range []int{12, 40, 90} {
			preview := turnPromptPreview(p.prompt, budget)
			for _, r := range preview {
				if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
					t.Errorf("%s budget=%d: control byte %#x survived into the preview %q", p.name, budget, r, preview)
				}
			}
			got := lipgloss.Width(preview)
			if got > budget {
				t.Errorf("%s budget=%d: preview width = %d (%q), want <= budget", p.name, budget, got, preview)
			}
			// And it must not waste the header either: a truncated preview should
			// land within one (wide) cell of the budget it was given.
			if got < budget-2 {
				t.Errorf("%s budget=%d: preview width = %d (%q), want it to fill the budget", p.name, budget, got, preview)
			}
		}
	}

	n, events := threeTurnRoot()
	m := &AgentsModel{}
	for _, p := range prompts {
		n.Turns[0].Prompt = p.prompt
		rows := m.detailRows(n, events)
		for _, w := range []int{80, 100, 120} {
			hdr := m.renderDetailRows(n, events, rows, w)[0]
			// A surviving newline would silently split one row into two and desync
			// the pane's line accounting.
			if strings.ContainsAny(stripANSITurns(hdr), "\n\r\t") {
				t.Errorf("%s w=%d: header row still carries a line/tab break: %q", p.name, w, stripANSITurns(hdr))
			}
			if lipgloss.Width(hdr) != w {
				t.Errorf("%s w=%d: header width = %d, want exactly %d", p.name, w, lipgloss.Width(hdr), w)
			}
			// fitLine guarantees the width above, so width alone is blind: an
			// oversized preview is absorbed by MaxWidth truncating from the RIGHT,
			// silently eating the duration/event-count suffix. Assert the suffix
			// survives verbatim.
			if plain := stripANSITurns(hdr); !strings.Contains(plain, "· 2 events") {
				t.Errorf("%s w=%d: header %q lost its event-count suffix to preview overflow", p.name, w, plain)
			}
		}
	}
}

// TestDetailRowsWidthInvariant: every rendered detail row is EXACTLY w cells,
// collapsed and expanded, at every pane width (learning
// border-inside-fixed-width-manual-join).
func TestDetailRowsWidthInvariant(t *testing.T) {
	n, events := threeTurnRoot()
	states := map[string]map[int]bool{
		"default":       nil,
		"all expanded":  {1: true, 2: true, 3: true},
		"all collapsed": {1: false, 2: false, 3: false},
	}
	for name, st := range states {
		for _, w := range []int{80, 100, 120} {
			for _, sel := range []int{0, 1, 2} {
				m := &AgentsModel{turnExpanded: st, rowSel: sel}
				rows := m.detailRows(n, events)
				for i, ln := range m.renderDetailRows(n, events, rows, w) {
					if got := lipgloss.Width(ln); got != w {
						t.Errorf("%s w=%d sel=%d: row %d width = %d, want %d (%q)",
							name, w, sel, i, got, w, ln)
					}
				}
			}
		}
	}
}

// --- navigation -----------------------------------------------------------

// detailModel builds a live AgentsModel focused on the detail pane of a
// three-turn root, primed by one render.
func detailModelWithTurns(t *testing.T) (*AgentsModel, []agent.AgentEvent) {
	t.Helper()
	tree := agent.NewAgentTree("root", "m")
	root := tree.Node(tree.RootID())

	n, events := threeTurnRoot()
	for _, e := range events {
		root.AddEvent(e)
	}
	root.Turns = append(root.Turns, n.Turns...)
	root.StartedAt = n.StartedAt
	root.Status = agent.StatusRunning

	m := NewAgentsModel(tree, nil)
	m.width, m.height = 120, 40
	m.inDetail = true
	m.selected = 0
	_ = m.View() // prime rowCount / row cache
	return m, events
}

func TestEnterOnHeaderTogglesGroupWithoutOpeningEventView(t *testing.T) {
	m, events := detailModelWithTurns(t)

	n := m.nodes[0]
	base := len(m.detailRows(n, events))

	// Select turn 1's header (row 0) via "g" — which also drops followTail, so
	// the next render will not snap the cursor back to the tail — then toggle.
	m.handleDetailKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'g'}})
	_ = m.View()
	if !m.detRows[m.rowSel].header {
		t.Fatalf("g landed on %+v, want turn 1's header", m.detRows[m.rowSel])
	}
	m.handleDetailKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m.inEventView {
		t.Fatal("enter on a turn header must NOT open the event view")
	}
	_ = m.View()

	grown := len(m.detailRows(n, events))
	if want := base + turnEventCounts[1]; grown != want {
		t.Errorf("after expanding turn 1: %d rows, want %d (grew by exactly turn 1's %d events)",
			grown, want, turnEventCounts[1])
	}
	// Selection stays on turn 1's header; eventSel stays a valid event index.
	if m.rowSel != 0 || !m.detRows[m.rowSel].header || m.detRows[m.rowSel].turn != 1 {
		t.Errorf("after toggle rowSel=%d points at %+v, want turn 1's header", m.rowSel, m.detRows[m.rowSel])
	}
	if m.eventSel < 0 || m.eventSel >= len(events) {
		t.Errorf("eventSel = %d, out of range [0,%d)", m.eventSel, len(events))
	}

	// Toggling again restores the collapsed row count.
	m.handleDetailKey(tea.KeyMsg{Type: tea.KeyEnter})
	_ = m.View()
	if got := len(m.detailRows(n, events)); got != base {
		t.Errorf("after re-collapsing turn 1: %d rows, want %d", got, base)
	}
	if m.rowSel != 0 || !m.detRows[0].header {
		t.Errorf("rowSel = %d after re-collapse, want turn 1's header at 0", m.rowSel)
	}
	if m.inEventView {
		t.Fatal("a second enter on a header must still not open the event view")
	}

	// Same again one row down: a toggle only ever inserts or removes rows BELOW
	// the header it acts on, so the cursor index must stay on that header.
	m.handleDetailKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	_ = m.View()
	if m.rowSel != 1 || m.detRows[1].turn != 2 || !m.detRows[1].header {
		t.Fatalf("rowSel=%d points at %+v, want turn 2's header", m.rowSel, m.detRows[m.rowSel])
	}
	m.handleDetailKey(tea.KeyMsg{Type: tea.KeyEnter})
	_ = m.View()
	if got, want := len(m.detRows), base+turnEventCounts[2]; got != want {
		t.Errorf("after expanding turn 2: %d rows, want %d", got, want)
	}
	if m.rowSel != 1 || m.detRows[1].turn != 2 || !m.detRows[1].header {
		t.Errorf("after toggling turn 2 rowSel=%d points at %+v, want turn 2's header",
			m.rowSel, m.detRows[m.rowSel])
	}
	if m.eventSel < 0 || m.eventSel >= len(events) {
		t.Errorf("eventSel = %d, out of range [0,%d)", m.eventSel, len(events))
	}
}

// TestEnterOnCurrentTurnHeaderIsSymmetric: the CURRENT turn is the one case
// where the header can also be the LAST row (once collapsed), so followTail is
// live on it. If the toggle leaves followTail set, the next render snaps the
// cursor onto the newly-revealed tail event and the following `enter` drills
// into an event instead of collapsing the group back — expand and collapse stop
// being symmetric. The cursor must stay on the header across both toggles.
func TestEnterOnCurrentTurnHeaderIsSymmetric(t *testing.T) {
	m, _ := detailModelWithTurns(t)

	// Park on turn 3's header (row 2) and collapse it, so it becomes the last row.
	m.rowSel = 2
	m.followTail = false
	_ = m.View()
	if !m.detRows[2].header || m.detRows[2].turn != 3 {
		t.Fatalf("row 2 = %+v, want turn 3's header", m.detRows[2])
	}
	m.handleDetailKey(tea.KeyMsg{Type: tea.KeyEnter})
	_ = m.View()
	if len(m.detRows) != 3 {
		t.Fatalf("after collapsing turn 3: %d rows, want 3 (headers only)", len(m.detRows))
	}

	// G is the natural way an operator reaches the bottom; it sets followTail.
	m.handleDetailKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'G'}})
	_ = m.View()
	if m.rowSel != 2 {
		t.Fatalf("G rowSel = %d, want turn 3's header at 2", m.rowSel)
	}

	// Expand: the cursor must NOT follow the tail into the revealed event rows.
	m.handleDetailKey(tea.KeyMsg{Type: tea.KeyEnter})
	_ = m.View()
	if got, want := len(m.detRows), 3+turnEventCounts[3]; got != want {
		t.Fatalf("after expanding turn 3: %d rows, want %d", got, want)
	}
	if m.rowSel != 2 || !m.detRows[m.rowSel].header || m.detRows[m.rowSel].turn != 3 {
		t.Fatalf("after expanding, rowSel=%d points at %+v, want turn 3's header at 2",
			m.rowSel, m.detRows[m.rowSel])
	}

	// ...so the very next enter collapses it again rather than drilling in.
	m.handleDetailKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m.inEventView {
		t.Fatal("the symmetric enter opened the event view instead of collapsing turn 3")
	}
	_ = m.View()
	if len(m.detRows) != 3 {
		t.Errorf("after re-collapsing turn 3: %d rows, want 3", len(m.detRows))
	}
	if m.rowSel != 2 || !m.detRows[2].header || m.detRows[2].turn != 3 {
		t.Errorf("after re-collapse rowSel=%d points at %+v, want turn 3's header at 2",
			m.rowSel, m.detRows[m.rowSel])
	}
}

func TestEnterOnEventRowOpensThatEvent(t *testing.T) {
	m, events := detailModelWithTurns(t)

	// Default state: rows are [h1, h2, h3, e5, e6]; the tail row is the last event.
	if got := len(m.detRows); got != 3+turnEventCounts[3] {
		t.Fatalf("primed row count = %d, want %d", got, 3+turnEventCounts[3])
	}
	m.rowSel = len(m.detRows) - 2 // turn 3's FIRST event row
	m.followTail = false          // as k/g would leave it
	_ = m.View()
	wantEvt := m.detRows[m.rowSel].evtIdx
	if wantEvt != len(events)-2 {
		t.Fatalf("row model resolved event %d, want %d", wantEvt, len(events)-2)
	}

	m.handleDetailKey(tea.KeyMsg{Type: tea.KeyEnter})
	if !m.inEventView {
		t.Fatal("enter on an event row must open the event view")
	}
	if m.eventSel != wantEvt {
		t.Fatalf("event view opened on event %d, want %d", m.eventSel, wantEvt)
	}
	out := stripANSITurns(strings.Join(m.buildDetailLines(100), "\n"))
	if !strings.Contains(out, events[wantEvt].Name) {
		t.Errorf("event view does not show event %q:\n%s", events[wantEvt].Name, out)
	}
}

func TestDetailNavigationMovesOverRowsIncludingHeaders(t *testing.T) {
	m, _ := detailModelWithTurns(t)

	m.handleDetailKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'g'}})
	_ = m.View()
	if m.rowSel != 0 || !m.detRows[0].header {
		t.Fatalf("g did not land on the first row (a header): rowSel=%d", m.rowSel)
	}
	// j from a header lands on the NEXT row, which is another header here.
	m.handleDetailKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	_ = m.View()
	if m.rowSel != 1 || !m.detRows[1].header {
		t.Fatalf("j from row 0 = %d, want row 1 (turn 2's header)", m.rowSel)
	}
	m.handleDetailKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	_ = m.View()
	if m.rowSel != 0 {
		t.Fatalf("k from row 1 = %d, want 0", m.rowSel)
	}
	// G pins the tail row, and the tail row's event is the newest event.
	m.handleDetailKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'G'}})
	_ = m.View()
	if m.rowSel != len(m.detRows)-1 {
		t.Fatalf("G rowSel = %d, want the last row %d", m.rowSel, len(m.detRows)-1)
	}
	if m.eventSel != m.eventCount-1 {
		t.Errorf("tail row eventSel = %d, want the newest event %d", m.eventSel, m.eventCount-1)
	}
}

// TestEventCountStillCountsEventsNotRows — other code reads m.eventCount.
func TestEventCountStillCountsEventsNotRows(t *testing.T) {
	m, events := detailModelWithTurns(t)
	if m.eventCount != len(events) {
		t.Errorf("eventCount = %d, want %d (EVENTS, not rows)", m.eventCount, len(events))
	}
	if m.rowCount != 3+turnEventCounts[3] {
		t.Errorf("rowCount = %d, want %d", m.rowCount, 3+turnEventCounts[3])
	}
}

// TestDetailPaneRowsFitPaneWidth renders through the real View() path and
// checks the composed frame keeps a constant width with headers present.
func TestDetailPaneWidthStableWithHeaders(t *testing.T) {
	m, _ := detailModelWithTurns(t)
	for _, w := range []int{80, 100, 120} {
		m.width = w
		frame := m.View()
		lines := strings.Split(frame, "\n")
		want := lipgloss.Width(lines[0])
		for i, ln := range lines {
			if got := lipgloss.Width(ln); got != want {
				t.Fatalf("w=%d: frame line %d width = %d, want %d", w, i, got, want)
			}
		}
	}
}
