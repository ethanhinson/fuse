package tui

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/reflow/wordwrap"
	"github.com/muesli/reflow/wrap"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/ratelimit"
)

// agentsExitMsg is returned to the parent ShellModel when AgentsModel exits.
type agentsExitMsg struct{}

// treeUpdateMsg signals that an agent tree update has arrived. Delivered by
// the StartBridges pump; no subscription command is needed.
type treeUpdateMsg struct{ nodeID string }

// AgentsModel renders the spatial agent tree overlay (Tab / /agents).
// Layout: 40% tree panel | 1-char divider | 60% detail panel.
//
// Key bindings per §4.2:
//
//	Tree:   j/k navigate  g/G first/last  enter/tab detail  b board  x cancel  esc exit
//	Detail: j/k scroll    g/G top/bottom  q/esc/tab back-to-tree
//	Board:  j/k scroll    g top          b/q/esc/tab back-to-tree
type AgentsModel struct {
	tree        *agent.AgentTree
	nodes       []agent.NodeView // depth-first snapshot, refreshed on tree update
	nodeByID    map[string]agent.NodeView
	lastChildOf map[string]string // parentID → last child's ID
	// treeRows is the left pane's row model (change turns-in-left-tree): turn
	// headers + nodes. m.selected indexes THIS, not m.nodes. Rebuilt every render.
	treeRows     []treeRow
	treeRowCount int
	selected     int
	treeScroll   int  // first visible row of the tree pane
	treeManual   bool // sticky: set by a wheel scroll, cleared by a selection key (j/k/g/G) or a pane enter/exit; while set, suppresses selection-follow scrolling
	inDetail     bool
	detailScroll int  // first visible event row in the detail list
	detailManual bool // sticky: set by a wheel scroll, cleared by a selection key (j/k/g/G) or a pane enter/exit; while set, suppresses selection-follow
	followTail   bool // selection sticks to the newest event until user moves up
	eventSel     int  // selected event (index into displayEvents)
	eventCount   int  // len(displayEvents) as of last render, for key clamping
	// Turn groups (change 0066). The detail pane's cursor is rowSel, an index into
	// the ROW model (headers included); eventSel stays an index into displayEvents
	// and is kept in lockstep whenever rowSel lands on an event row, because
	// buildEventViewLines indexes `visible` with it. For a single-turn root or any
	// child node the row model is 1:1 with the events, so rowSel == eventSel and
	// every behavior below is exactly today's.
	rowSel   int         // selected detail ROW (index into detRows)
	rowCount int         // len(detRows) as of last render, for key clamping
	detRows  []detailRow // last rendered row model; read by enter/toggle
	// turnExpanded is the per-turn collapse state, keyed by conversational
	// ordinal. EPHEMERAL — never persisted. Absent means the default (every turn
	// but the last collapsed), so the map only ever holds turns the operator
	// explicitly toggled.
	turnExpanded map[int]bool
	inEventView  bool // full-content view of the selected event
	eventScroll  int  // scroll offset within the expanded event
	// inBlackboard shows the session blackboard snapshot in the detail pane
	// (change 0023); bbScroll is the first visible line of that view.
	inBlackboard bool
	bbScroll     int
	blackboard   *agent.Blackboard // session blackboard, or nil (no /agents bb tab)
	width        int
	height       int
	// rateGate is the session's shared rate-gate bucket, or nil (change 0036). Its
	// live utilization joins the scheduler summary block at the top of the tree
	// pane so the overlay shows the throughput brake alongside the pool counters.
	rateGate *ratelimit.Bucket

	// segmentsDir is the session's segments/ directory (change 0030), or "" when
	// segment archiving is off. The header reads its index.json (cached) to show
	// the segments indicator, and "s" reads the archived raw regions into the
	// existing event drill-in. segCache/segCacheMod cache the last parsed index so
	// the per-render header read is not a fresh disk hit every frame.
	segmentsDir   string
	segCache      *segmentSummaryData
	segCacheMod   time.Time
	inSegmentView bool               // showing archived raw region in the drill-in
	segEvents     []agent.AgentEvent // synthetic single event feeding buildEventViewLines
}

// segmentSummaryData is the cached, parsed segments summary for the header
// indicator: the segment count and total tokens before/after.
type segmentSummaryData struct {
	count        int
	tokensBefore int
	tokensAfter  int
}

// NewAgentsModel creates an AgentsModel backed by the given tree, with an
// optional rate-gate bucket for the scheduler summary block (nil = no gate).
func NewAgentsModel(t *agent.AgentTree, gate *ratelimit.Bucket) *AgentsModel {
	m := &AgentsModel{tree: t, rateGate: gate, followTail: true}
	m.refreshSnapshot()
	return m
}

// WithBlackboard attaches the session blackboard so the overlay can show its
// snapshot in a Blackboard section (change 0023). Nil is the no-blackboard path:
// the "b" key then reports the tab is unavailable.
func (m *AgentsModel) WithBlackboard(bb *agent.Blackboard) *AgentsModel {
	m.blackboard = bb
	return m
}

// WithSegmentsDir attaches the session's segments/ directory (change 0030) so
// the detail header can show the compaction indicator and "s" can open the
// archived raw region in the drill-in. Empty ("") is the no-segments path.
func (m *AgentsModel) WithSegmentsDir(dir string) *AgentsModel {
	m.segmentsDir = dir
	return m
}

// ShowBlackboard opens the overlay directly on the Blackboard section, as if the
// user had pressed "b". Used by the /blackboard slash command so it lands on the
// board rather than the tree list. Returns the receiver for chaining.
func (m *AgentsModel) ShowBlackboard() *AgentsModel {
	m.inBlackboard = true
	m.bbScroll = 0
	return m
}

// SelectedNodeID returns the ID of the currently-highlighted tree node, or ""
// when the snapshot is empty. Used by the human-messaging router to default
// bare-prose targets to the node the human is looking at (ADR-0022).
func (m *AgentsModel) SelectedNodeID() string {
	if n, ok := m.selectedNode(); ok {
		return n.ID
	}
	return ""
}

// selectedNode resolves m.selected (a treeRows index) to the node under the
// cursor. A turn HEADER row resolves to the ROOT node (its detail is the root's
// turn-scoped transcript; see selectedTurnFilter). Falls back to the flat m.nodes
// index when the row model has not been built yet (first frame).
func (m *AgentsModel) selectedNode() (agent.NodeView, bool) {
	if m == nil || m.selected < 0 {
		return agent.NodeView{}, false
	}
	if m.selected < len(m.treeRows) {
		tr := m.treeRows[m.selected]
		// A header carries the root node (tr.node) so it is inspectable as the
		// root's turn slice; a node row carries its own node.
		if tr.nodeIdx < 0 {
			return agent.NodeView{}, false
		}
		return tr.node, true
	}
	// Pre-first-render fallback: m.treeRows is empty but m.nodes is populated.
	if len(m.treeRows) == 0 && m.selected < len(m.nodes) {
		return m.nodes[m.selected], true
	}
	return agent.NodeView{}, false
}

// selectedTurnFilter returns the conversational turn ordinal the detail pane
// should scope the selected node's events to, or 0 for "no filter" (show the
// whole node). A turn HEADER row scopes the root's events to that turn; a node
// row (a spawned agent, or a single-turn root) shows everything.
func (m *AgentsModel) selectedTurnFilter() int {
	if m == nil || m.selected < 0 || m.selected >= len(m.treeRows) {
		return 0
	}
	tr := m.treeRows[m.selected]
	if tr.header {
		return tr.turn
	}
	return 0
}

// rightFocused reports whether the right column (detail/blackboard/event/segment)
// currently holds focus, derived entirely from the existing view-state flags —
// there is no separate focus source of truth. False means the left (tree) column
// is focused. The colored-border focus indicator and the wheel-scroll routing
// both key off this.
func (m *AgentsModel) rightFocused() bool {
	return m.inDetail || m.inBlackboard || m.inEventView || m.inSegmentView
}

// Init is a no-op; the parent ShellModel owns the tree-update subscription.
func (m *AgentsModel) Init() tea.Cmd { return nil }

// Update handles key events and tree updates forwarded from ShellModel.
func (m *AgentsModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
	case treeUpdateMsg:
		m.refreshSnapshot()
	case tea.MouseMsg:
		m.handleMouse(msg)
	case tea.KeyMsg:
		// Approval popups are owned by ShellModel (which intercepts keys while
		// its queue is non-empty), so only navigation keys arrive here.
		if m.inBlackboard {
			return m.handleBlackboardKey(msg)
		}
		// "b" is a global swap into the Blackboard from the tree list OR a node's
		// detail view — the board is session-scoped, so it should be reachable
		// wherever you are in the overlay, not only from the top-level tree.
		// (The expanded single-event reader keeps "b" for its own scrolling.)
		if msg.String() == "b" && !m.inEventView && !m.inSegmentView {
			m.inBlackboard = true
			m.bbScroll = 0
			return m, nil
		}
		if m.inSegmentView {
			return m.handleSegmentViewKey(msg)
		}
		if m.inEventView {
			return m.handleEventViewKey(msg)
		}
		if m.inDetail {
			return m.handleDetailKey(msg)
		}
		return m.handleTreeKey(msg)
	}
	return m, nil
}

// View renders the full-screen two-pane layout via manual line-by-line joining.
// We avoid lipgloss.JoinHorizontal because it can't constrain line widths when
// ANSI-styled content exceeds the allocated column width.
func (m *AgentsModel) View() string {
	if m.width < 20 || m.height < 4 {
		return "agents"
	}
	treeW := m.width * 40 / 100
	detailW := m.width - treeW - 1 // 1 for the divider column

	// Scheduler summary header (change 0036) spans the full overlay width above the
	// two-panel split so the spec-shape per-pool lines are not clipped by the narrow
	// tree column. It costs rows, so the panels build to m.height minus its height —
	// temporarily shrinking m.height keeps each panel's own help bar visible rather
	// than dropping it off the bottom.
	//
	// Clamp the header to at most m.height-1 lines so the panels always get at least
	// one row (h >= 1) and the total output (len(header) + h) never exceeds the
	// overlay height (N-4): an unbounded header on a very short overlay would
	// otherwise force h=1 while the header alone already overflows the height.
	header := m.schedulerHeaderLines(m.width)
	if len(header) > m.height-1 {
		header = header[:m.height-1]
	}
	h := m.height - len(header)
	if h < 1 {
		h = 1
	}

	// Colored-border focus indicator (change 0041). Each pane is boxed by manual
	// border glyphs drawn INSIDE the existing fit-line width budget, so the join
	// shape and exact-width invariant are unchanged. The border costs 2 rows
	// (top+bottom) per pane. The center divider column is SUBSUMED as the shared
	// vertical seam: the tree pane draws only its left border and the detail pane
	// only its right border, and the existing 1-col divChar between them serves as
	// the single interior seam — so no doubled vertical bars. That means each pane
	// loses exactly one interior column (its own outer border glyph), so the
	// content budget shrinks by 1 per pane, not 2.
	treeContentW := treeW - 1 // left border glyph only
	if treeContentW < 1 {
		treeContentW = 1
	}
	detailContentW := detailW - 1 // right border glyph only
	if detailContentW < 1 {
		detailContentW = 1
	}
	contentH := h - 2
	if contentH < 1 {
		contentH = 1
	}

	// Thread the reduced content height through m.height exactly as the scheduler
	// header shrink does: build*Lines read m.height internally.
	fullHeight := m.height
	m.height = contentH
	treeLines := m.buildTreeLines(treeContentW)
	detailLines := m.buildDetailLines(detailContentW)
	m.height = fullHeight
	for len(treeLines) < contentH {
		treeLines = append(treeLines, strings.Repeat(" ", treeContentW))
	}
	for len(detailLines) < contentH {
		detailLines = append(detailLines, strings.Repeat(" ", detailContentW))
	}

	// Which side is hot: the focused pane's border/title use the accent color, the
	// other muted.
	treeAccent, detailAccent := colCyan, colMuted
	if m.rightFocused() {
		treeAccent, detailAccent = colMuted, colCyan
	}
	// Tree pane: left border only (drawRight=false). Detail pane: right border only
	// (drawLeft=false). The seam between them is the shared divChar.
	treePane := boxPane(treeLines, treeW, contentH, "agents", treeAccent, true, false)
	detailPane := boxPane(detailLines, detailW, contentH, m.detailTitle(), detailAccent, false, true)

	divChar := lipgloss.NewStyle().Foreground(colMuted).Render("│")
	lines := make([]string, 0, len(header)+h)
	lines = append(lines, header...)
	for i := 0; i < h; i++ {
		lines = append(lines, fitLine(treePane[i], treeW)+divChar+fitLine(detailPane[i], detailW))
	}

	var b strings.Builder
	for i, line := range lines {
		b.WriteString(line)
		if i < len(lines)-1 {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// detailTitle names the right pane by its current sub-view, for the border top.
func (m *AgentsModel) detailTitle() string {
	switch {
	case m.inBlackboard:
		return "blackboard"
	case m.inSegmentView:
		return "segment"
	case m.inEventView:
		return "event"
	case m.inDetail:
		return "detail"
	default:
		return "detail"
	}
}

// boxPane wraps contentLines in a manual box border colored by `border`, returning
// exactly contentH+2 lines each paneW cells wide. drawLeft/drawRight select which
// vertical edges this pane draws — the center split subsumes the shared divider by
// having the left pane draw only its left edge and the right pane only its right
// edge, so the interior seam is a single glyph, not a doubled bar. The border
// glyphs live inside the pane's own width budget so the caller's
// fitLine(...)+divChar+fitLine(...) join keeps the exact total width. The title is
// embedded in the top border row in the same color. Nothing here re-flows content;
// each content line is padded to the interior width and framed.
func boxPane(contentLines []string, paneW, contentH int, title string, border lipgloss.Color, drawLeft, drawRight bool) []string {
	bs := lipgloss.NewStyle().Foreground(border)
	edges := 0
	if drawLeft {
		edges++
	}
	if drawRight {
		edges++
	}
	inner := paneW - edges // columns available for content between vertical glyphs
	if inner < 1 {
		inner = 1
	}

	leftTop, rightTop, leftBot, rightBot := "", "", "", ""
	left, right := "", ""
	if drawLeft {
		leftTop, leftBot, left = "┌", "└", "│"
	}
	if drawRight {
		rightTop, rightBot, right = "┐", "┘", "│"
	}

	top := leftTop + fitBorderTop(title, inner) + rightTop
	bottom := leftBot + strings.Repeat("─", inner) + rightBot

	out := make([]string, 0, contentH+2)
	out = append(out, bs.Render(top))
	lg, rg := bs.Render(left), bs.Render(right)
	for i := 0; i < contentH; i++ {
		var c string
		if i < len(contentLines) {
			c = contentLines[i]
		}
		out = append(out, lg+fitLine(c, inner)+rg)
	}
	out = append(out, bs.Render(bottom))
	return out
}

// fitBorderTop builds the interior of a box's top border for a run of `inner`
// cells: a title embedded between dashes as ─ title ─────, padded/truncated so the
// interior is exactly `inner` cells wide (excludes the two corner glyphs).
func fitBorderTop(title string, inner int) string {
	title = sanitizeDisplay(title)
	// "─ " + title + " " leader, then fill the rest with ─.
	lead := "─ "
	tail := " "
	base := lead + title + tail
	bw := lipgloss.Width(base)
	if bw >= inner {
		// Title too wide for the border: just fill with dashes.
		return strings.Repeat("─", inner)
	}
	return base + strings.Repeat("─", inner-bw)
}

// ─── key handlers ─────────────────────────────────────────────────────────────

func (m *AgentsModel) handleTreeKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// m.selected indexes the row model (turn headers + nodes). treeRowCount is the
	// count from the last render; on the very first key before a render it is 0, so
	// fall back to the node count.
	n := m.treeRowCount
	if n == 0 {
		n = len(m.nodes)
	}
	switch msg.String() {
	case "j", "down":
		if m.selected < n-1 {
			m.selected++
		}
		m.treeManual = false // selection keys re-engage selection-follow scrolling
	case "k", "up":
		if m.selected > 0 {
			m.selected--
		}
		m.treeManual = false
	case "g":
		m.selected = 0
		m.treeManual = false
	case "G":
		if n > 0 {
			m.selected = n - 1
		}
		m.treeManual = false
	case "enter", "tab":
		// On a turn HEADER row: if the turn has spawned agents, enter/tab toggles
		// the group (reveal/hide them). If it has NONE (the root worked directly),
		// there is nothing to toggle — so enter/tab drills into the detail pane
		// instead, focusing the root's turn-scoped transcript so its events can be
		// scrolled and expanded (change: header-enter-drills-when-childless).
		if m.selected >= 0 && m.selected < len(m.treeRows) && m.treeRows[m.selected].header {
			tr := m.treeRows[m.selected]
			if m.turnHasChildren(tr.turn) {
				m.toggleTurn(tr.turn, m.isLastTreeTurn(tr.turn))
				m.treeManual = false
				break
			}
			// fall through to the detail-pane drill-in below
		}
		m.inDetail = true
		m.detailScroll = 0
		m.eventSel = 0
		m.detailManual = false // clear any stale wheel flag so followTail lands on tail
		m.followTail = true    // land on the newest event of the chosen node
		m.rowSel = 0
		m.detRows, m.rowCount = nil, 0
	case "x":
		if node, ok := m.selectedNode(); ok {
			m.tree.CancelNode(node.ID)
		}
	// "b" (enter Blackboard) is handled globally in Update, before sub-view
	// dispatch, so it works from the detail view too — not repeated here.
	case "q", "esc":
		return m, func() tea.Msg { return agentsExitMsg{} }
	}
	return m, nil
}

// handleBlackboardKey scrolls the blackboard snapshot; b/q/esc/tab returns to
// the tree.
func (m *AgentsModel) handleBlackboardKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "j", "down":
		m.bbScroll++
	case "k", "up":
		if m.bbScroll > 0 {
			m.bbScroll--
		}
	case "g":
		m.bbScroll = 0
	case "n":
		// Jump to the next writer group's first line (clamps at the last group).
		if starts := m.blackboardGroupStarts(); len(starts) > 0 {
			for _, s := range starts {
				if s > m.bbScroll {
					m.bbScroll = s
					break
				}
			}
		}
	case "p":
		// Jump to the previous writer group's first line (clamps at the first).
		if starts := m.blackboardGroupStarts(); len(starts) > 0 {
			target := starts[0]
			for _, s := range starts {
				if s < m.bbScroll {
					target = s
				} else {
					break
				}
			}
			m.bbScroll = target
		}
	case "b", "q", "esc", "tab":
		m.inBlackboard = false
		m.bbScroll = 0
	}
	return m, nil
}

// blackboardGroupStarts returns the body-line index of each writer-group header
// for the current snapshot, reusing the same grouping as the render path so n/p
// navigation lands exactly on a group's first line. Empty when there is no board.
func (m *AgentsModel) blackboardGroupStarts() []int {
	if m.blackboard == nil {
		return nil
	}
	snap := m.blackboard.Snapshot()
	if len(snap) == 0 {
		return nil
	}
	// Compute at the SAME content width the render uses so wrapped-line counts (and
	// thus group-start indices) match exactly: the detail pane is 60% of the width
	// minus the divider column, minus 1 for its single border glyph.
	_, starts := m.blackboardBody(snap, m.blackboardContentWidth())
	return starts
}

// blackboardContentWidth returns the content width the blackboard body is built
// to during View() — the detail pane width (60% split minus the divider) minus the
// pane's single border glyph. Mirrors the width math in View().
func (m *AgentsModel) blackboardContentWidth() int {
	treeW := m.width * 40 / 100
	detailW := m.width - treeW - 1
	w := detailW - 1 // single (right) border glyph
	if w < 1 {
		w = 1
	}
	return w
}

func (m *AgentsModel) handleDetailKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// j/k/g/G move the ROW cursor (turn headers included, change 0066). With a
	// single-turn root or a child node the row model is 1:1 with the events, so
	// rowSel is the event index and these are today's semantics unchanged;
	// eventSel is re-derived from the row at render.
	switch msg.String() {
	case "j", "down":
		if m.rowSel < m.rowCount-1 {
			m.rowSel++
		}
		m.followTail = m.rowSel >= m.rowCount-1
		m.detailManual = false // selection keys re-engage selection-follow scrolling
	case "k", "up":
		if m.rowSel > 0 {
			m.rowSel--
		}
		m.followTail = false
		m.detailManual = false
	case "g":
		m.rowSel = 0
		m.followTail = false
		m.detailManual = false
	case "G":
		m.followTail = true
		m.detailManual = false
	case "enter":
		// The detail pane is a flat per-node transcript (turn grouping lives in the
		// left tree now), so enter always drills into the selected event.
		if m.eventCount > 0 {
			m.inEventView = true
			m.eventScroll = 0
		}
	case "s":
		// Show original (change 0030): open the archived raw region in the existing
		// event drill-in (it already sanitizes). Only meaningful when this session
		// has archived segments (root node). A no-op otherwise.
		if raw := m.loadSegmentRegions(); raw != "" {
			m.segEvents = []agent.AgentEvent{{
				Kind:    agent.KindToolResult,
				Name:    "archived raw region",
				Payload: map[string]any{"output": raw},
			}}
			m.inSegmentView = true
			m.eventScroll = 0
		}
	case "q", "esc", "tab":
		m.inDetail = false
		m.detailScroll = 0
		m.detailManual = false // returning to the tree clears the wheel flag
		m.treeManual = false   // re-engage the tree's selection-follow scrolling
		m.followTail = true
	}
	return m, nil
}

// handleSegmentViewKey scrolls the archived raw-region drill-in and backs out to
// the detail event list on esc/q/tab (change 0030). It reuses the event-view
// scroll semantics so the reader behaves identically to the normal drill-in.
func (m *AgentsModel) handleSegmentViewKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "esc", "tab":
		m.inSegmentView = false
		m.segEvents = nil
		m.eventScroll = 0
		return m, nil
	default:
		return m.handleEventViewKey(msg)
	}
}

// handleEventViewKey scrolls within one expanded event.
func (m *AgentsModel) handleEventViewKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "j", "down":
		m.eventScroll++
	case "k", "up":
		if m.eventScroll > 0 {
			m.eventScroll--
		}
	case "g":
		m.eventScroll = 0
	case "G":
		m.eventScroll = 1 << 30 // clamped at render time
	case "q", "esc", "tab":
		m.inEventView = false
		m.eventScroll = 0
	}
	return m, nil
}

// wheelStep is the number of lines the mouse wheel scrolls per event.
const wheelStep = 3

// handleMouse: the wheel scrolls the FOCUSED pane's OFFSET (not its selection),
// keyed off the current view-state flags (change 0041). The selection cursor is
// moved only by j/k/arrows now; the wheel never drags it. Each offset is floored
// at 0 here and clamped to its content's upper bound at render time by the
// corresponding build*Lines (which re-clamp every frame), so the view can't scroll
// past content in either direction.
func (m *AgentsModel) handleMouse(msg tea.MouseMsg) {
	up := msg.Button == tea.MouseButtonWheelUp
	down := msg.Button == tea.MouseButtonWheelDown
	if !up && !down {
		return
	}
	delta := wheelStep
	if up {
		delta = -wheelStep
	}
	switch {
	case m.inEventView, m.inSegmentView:
		// Expanded event / archived segment region both scroll eventScroll.
		m.eventScroll += delta
		if m.eventScroll < 0 {
			m.eventScroll = 0
		}
	case m.inBlackboard:
		m.bbScroll += delta
		if m.bbScroll < 0 {
			m.bbScroll = 0
		}
	case m.inDetail:
		m.detailManual = true
		m.detailScroll += delta
		if m.detailScroll < 0 {
			m.detailScroll = 0
		}
	default: // tree pane (left focus)
		m.treeManual = true
		m.treeScroll += delta
		if m.treeScroll < 0 {
			m.treeScroll = 0
		}
	}
}

// ─── tree panel ───────────────────────────────────────────────────────────────

// buildTreeLines returns m.height lines for the tree panel (last line = help
// bar), scrolled so the selected node is always visible — big trees overflow
// the pane and must window, not clip.
func (m *AgentsModel) buildTreeLines(w int) []string {
	rows := m.renderTreeRows(w)
	help := fitHelp(
		"j/k select  enter inspect  b board  x cancel  esc exit",
		"j/k · enter · b board · x · esc",
		w,
	)

	visRows := m.height - 1
	if visRows < 1 {
		visRows = 1
	}
	// Keep the selection inside the window — unless the wheel is driving the scroll
	// (treeManual), in which case the offset is honored as-is and the selection may
	// sit off-screen (change 0041: the wheel scrolls the view, not the cursor).
	if !m.treeManual {
		if m.selected < m.treeScroll {
			m.treeScroll = m.selected
		}
		if m.selected >= m.treeScroll+visRows {
			m.treeScroll = m.selected - visRows + 1
		}
	}
	if maxScroll := len(rows) - visRows; m.treeScroll > maxScroll {
		m.treeScroll = maxScroll
	}
	if m.treeScroll < 0 {
		m.treeScroll = 0
	}
	end := m.treeScroll + visRows
	if end > len(rows) {
		end = len(rows)
	}
	window := rows[m.treeScroll:end]

	// Overflow indicators so a windowed tree doesn't read as the whole tree —
	// but never at the cost of hiding the selected row itself.
	if m.treeScroll > 0 && m.selected != m.treeScroll {
		window[0] = fitLine(treeHelpStyle.Render(fmt.Sprintf("↑ %d more…", m.treeScroll)), w)
	}
	if hidden := len(rows) - end; hidden > 0 && m.selected != end-1 {
		window[len(window)-1] = fitLine(treeHelpStyle.Render(fmt.Sprintf("↓ %d more…", hidden)), w)
	}

	out := append([]string{}, window...)
	for len(out) < m.height-1 {
		out = append(out, "")
	}
	return append(out[:m.height-1], help)
}

// schedulerHeaderLines renders the scheduler observability block spanning the
// full overlay width w: one line per engaged pool and per rate-gate provider
// (change 0036), each fit to w and styled like the help bar. Empty when nothing
// is engaged, so an idle overlay shows no header. Rendered above the two-panel
// split so the spec-shape lines are not clipped by the narrow tree column.
func (m *AgentsModel) schedulerHeaderLines(w int) []string {
	if m.tree == nil {
		return nil
	}
	lines := schedulerSummaryLines(m.tree.Scheduler().Snapshot(), m.rateGate.Utilization())
	if len(lines) == 0 {
		return nil
	}
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		out = append(out, fitLine(treeHelpStyle.Render(l), w))
	}
	return out
}

// treeRow is one row of the left pane's row model (change turns-in-left-tree):
// either a collapsible TURN header or a node. m.selected indexes this model, not
// m.nodes, so a header is a first-class, selectable row.
type treeRow struct {
	header  bool           // true = a "▾ turn N · prompt" header
	turn    int            // the conversational ordinal (header row, or a node's group)
	nodeIdx int            // index into m.nodes; -1 for a header row
	node    agent.NodeView // the node for a node row (zero for a header)
}

// buildTreeRowModel groups the flat node snapshot into per-turn sections when the
// root is genuinely multi-turn. Turns are the TOP level: each turn is a
// selectable, collapsible header whose detail is the ROOT's events for THAT turn
// (the root does most work itself, so a turn is meaningless without its root
// slice). Any agents spawned in that turn nest under it. There is NO standalone
// root row — the root's activity lives inside its turns. A single-turn (or
// turn-less) session returns the flat node list unchanged, so the pre-change tree
// is byte-identical.
func (m *AgentsModel) buildTreeRowModel() []treeRow {
	// Find the root node (index + view) and its turn marks.
	rootIdx, rootFound := -1, false
	var rootTurns []agent.TurnMark
	for i, n := range m.nodes {
		if n.Depth == 0 {
			rootIdx, rootFound = i, true
			rootTurns = n.Turns
			break
		}
	}
	multiTurn := rootFound && len(rootTurns) > 1

	if !multiTurn {
		rows := make([]treeRow, len(m.nodes))
		for i, n := range m.nodes {
			rows[i] = treeRow{turn: n.SpawnedInTurn, nodeIdx: i, node: n}
		}
		return rows
	}

	root := m.nodes[rootIdx]
	rows := make([]treeRow, 0, len(m.nodes)+len(rootTurns))
	// One section per turn: a header that IS the root's turn-N slice (selectable),
	// followed by that turn's spawned children (unless collapsed).
	for ti := range rootTurns {
		turn := rootTurns[ti].Turn
		if turn == 0 {
			turn = ti + 1
		}
		// The header carries the root node + turn: selecting it inspects the root's
		// events scoped to this turn (nodeIdx points at the root so cancel/other
		// node ops still resolve).
		rows = append(rows, treeRow{header: true, turn: turn, nodeIdx: rootIdx, node: root})
		if !m.turnIsExpanded(turn, ti == len(rootTurns)-1) {
			continue
		}
		for i, n := range m.nodes {
			if n.Depth == 0 {
				continue
			}
			if n.SpawnedInTurn == turn {
				rows = append(rows, treeRow{turn: turn, nodeIdx: i, node: n})
			}
		}
	}
	return rows
}

func (m *AgentsModel) renderTreeRows(w int) []string {
	if len(m.nodes) == 0 {
		return []string{lipgloss.NewStyle().Foreground(colMuted).Render("No agents yet.")}
	}
	m.treeRows = m.buildTreeRowModel()
	m.treeRowCount = len(m.treeRows)
	rows := make([]string, 0, len(m.treeRows))
	for i, tr := range m.treeRows {
		if tr.header {
			rows = append(rows, m.renderTurnHeaderRow(tr, i == m.selected, w))
			continue
		}
		rows = append(rows, m.renderNodeRow(tr.node, i == m.selected, w))
	}
	return rows
}

// renderTurnHeaderRow renders a turn header to w cells. A caret (▾/▸) is shown
// ONLY when the turn has spawned agents to reveal; a turn where the root worked
// directly (no children) shows a plain bullet, so the operator is not invited to
// "expand" a group that has nothing under it.
func (m *AgentsModel) renderTurnHeaderRow(tr treeRow, selected bool, w int) string {
	glyph := "·" // no children: nothing to expand, just a selectable turn row
	if m.turnHasChildren(tr.turn) {
		glyph = "▾"
		isLast := m.isLastTreeTurn(tr.turn)
		if !m.turnIsExpanded(tr.turn, isLast) {
			glyph = "▸"
		}
	}
	prompt := m.turnPromptForOrdinal(tr.turn)
	label := glyph + " turn " + fmt.Sprintf("%d", tr.turn)
	if prompt != "" {
		label += " · " + prompt
	}
	plain := fitLine(label, w)
	if selected {
		return fitLine(lipgloss.NewStyle().Reverse(true).Render(fitLine(label, w)), w)
	}
	return lipgloss.NewStyle().Foreground(colCyan).Bold(true).Render(plain)
}

// turnHasChildren reports whether any spawned agent belongs to this turn (a node
// with SpawnedInTurn == turn). Only such turns get an expand/collapse caret.
func (m *AgentsModel) turnHasChildren(turn int) bool {
	for _, n := range m.nodes {
		if n.Depth != 0 && n.SpawnedInTurn == turn {
			return true
		}
	}
	return false
}

// isLastTreeTurn reports whether `turn` is the highest turn ordinal on the root.
func (m *AgentsModel) isLastTreeTurn(turn int) bool {
	max := 0
	for _, n := range m.nodes {
		if n.Depth == 0 {
			for _, tm := range n.Turns {
				o := tm.Turn
				if o > max {
					max = o
				}
			}
		}
	}
	return turn == max
}

// turnPromptForOrdinal returns the (sanitized, truncated) prompt for a turn.
func (m *AgentsModel) turnPromptForOrdinal(turn int) string {
	for _, n := range m.nodes {
		if n.Depth == 0 {
			for _, tm := range n.Turns {
				o := tm.Turn
				if o == 0 {
					continue
				}
				if o == turn {
					return turnPromptPreview(tm.Prompt, 48)
				}
			}
		}
	}
	return ""
}

// renderNodeRow renders one tree row to exactly w cells.
// Three zones: [edge+cloud+label] [dot fill] [glyph+elapsed+tokens]
func (m *AgentsModel) renderNodeRow(n agent.NodeView, selected bool, w int) string {
	edge := m.edgePrefix(n)
	label := sanitizeDisplay(n.Label) // model-controlled; tabs/controls shear columns
	cloud := ""

	glyph := glyphForStatus(n.Status)
	elapsed := nodeElapsed(n)
	tokens := ""
	if n.TokensIn > 0 || n.TokensOut > 0 {
		tokens = " ↑" + formatTokens(n.TokensIn) + " ↓" + formatTokens(n.TokensOut)
	}
	right := glyph + " " + elapsed + tokens

	prefixPlain := edge + cloud + label
	leftW := lipgloss.Width(prefixPlain)
	rightW := lipgloss.Width(right)

	dotCount := w - leftW - rightW - 4
	if dotCount < 1 {
		dotCount = 1
	}
	dots := strings.Repeat("─", dotCount)

	if selected {
		plain := prefixPlain + "  " + dots + "  " + right
		return fitLine(lipgloss.NewStyle().Reverse(true).Render(plain), w)
	}

	edgeS := lipgloss.NewStyle().Foreground(colMuted).Render(edge)
	cloudS := ""
	if cloud != "" {
		cloudS = lipgloss.NewStyle().Foreground(colAmber).Render(cloud)
	}
	labelS := lipgloss.NewStyle().Foreground(colNormal).Render(label)
	dotsS := lipgloss.NewStyle().Foreground(colMuted).Render("  " + dots + "  ")
	glyphS := glyphStyle(n.Status).Render(glyph)
	rightS := glyphS + lipgloss.NewStyle().Foreground(colMuted).Render(" "+elapsed+tokens)

	return fitLine(edgeS+cloudS+labelS+dotsS+rightS, w)
}

// edgePrefix builds the box-drawing edge for n using isLastChild lookups.
func (m *AgentsModel) edgePrefix(n agent.NodeView) string {
	if n.Depth == 0 {
		return ""
	}
	var chain []agent.NodeView
	cur, ok := m.nodeByID[n.ParentID]
	for ok && cur.Depth > 0 {
		chain = append([]agent.NodeView{cur}, chain...)
		cur, ok = m.nodeByID[cur.ParentID]
	}
	prefix := ""
	for _, anc := range chain {
		if m.lastChildOf[anc.ParentID] == anc.ID {
			prefix += "    "
		} else {
			prefix += "│   "
		}
	}
	if m.lastChildOf[n.ParentID] == n.ID {
		prefix += "└─ "
	} else {
		prefix += "├─ "
	}
	return prefix
}

// ─── detail panel ─────────────────────────────────────────────────────────────

// displayEvents filters the raw event list to the rows the detail pane shows
// (token events are noise); m.eventSel indexes into THIS slice.
func displayEvents(events []agent.AgentEvent) []agent.AgentEvent {
	out := events[:0:0]
	for _, e := range events {
		if e.Kind != agent.KindTokens {
			out = append(out, e)
		}
	}
	return out
}

// buildDetailLines returns m.height lines for the detail panel: either the
// event list (one selectable row per event) or, after enter, the full
// word-wrapped content of the selected event.
func (m *AgentsModel) buildDetailLines(w int) []string {
	if m.inBlackboard {
		return m.buildBlackboardLines(w)
	}
	if len(m.nodes) == 0 {
		return []string{lipgloss.NewStyle().Foreground(colMuted).Render("Select a node to inspect.")}
	}
	n, ok := m.selectedNode()
	if !ok {
		return []string{lipgloss.NewStyle().Foreground(colMuted).Render("Select a node to inspect.")}
	}
	// Events aren't carried in the snapshot (they can be large); take a
	// race-safe copy from the live node only for the selected one.
	var events []agent.AgentEvent
	if live := m.tree.Node(n.ID); live != nil {
		events = live.CopyEvents()
	}
	visible := displayEvents(events)
	// A turn HEADER selection scopes the root's transcript to that turn (the root
	// does most work itself, so a turn's meaning is its slice of the root stream).
	// turnIndexFor is the same attribution rule the offsets use.
	if tf := m.selectedTurnFilter(); tf > 0 && len(n.Turns) > 1 {
		filtered := visible[:0:0]
		for _, e := range visible {
			if turnOrdinal(n, turnIndexFor(n, e.TS)) == tf {
				filtered = append(filtered, e)
			}
		}
		visible = filtered
	}
	m.eventCount = len(visible)

	if m.inEventView && len(visible) > 0 {
		return m.buildEventViewLines(n, visible, w)
	}
	m.inEventView = false

	// "s" show-original: render the archived raw region through the SAME event
	// drill-in (it already sanitizes — learning sanitize-untrusted-bytes-fixed-
	// width-tui — so no new render path). segEvents is a synthetic single event
	// holding the raw region text.
	if m.inSegmentView && len(m.segEvents) > 0 {
		m.eventSel = 0
		return m.buildEventViewLines(n, m.segEvents, w)
	}
	m.inSegmentView = false

	headerLines := strings.Split(m.renderDetailHeader(n, w), "\n")
	rule := lipgloss.NewStyle().Foreground(colMuted).Render(strings.Repeat("─", w))

	rows := m.height - 2 - len(headerLines) // header line(s) + rule + help
	if rows < 1 {
		rows = 1
	}

	var evtLines []string
	if len(visible) == 0 {
		m.detRows, m.rowCount = nil, 0
		// An eventless pane reads as "broken"; say what the node is doing.
		muted := lipgloss.NewStyle().Foreground(colMuted)
		switch n.Status {
		case agent.StatusPending:
			evtLines = []string{muted.Render("Queued — waiting for a spawn slot (" +
				fmt.Sprintf("%d concurrent max", agent.MaxConcurrentSpawns) + ")…")}
		case agent.StatusRunning:
			evtLines = []string{muted.Render("Starting — no events yet…")}
		case agent.StatusCancelled:
			evtLines = []string{muted.Render("Cancelled before producing events.")}
		}
	} else {
		// The ROW model is the pane's source of truth (change 0066): for a
		// single-turn root or a child node it is 1:1 with the events, so rowSel is
		// the event index and everything below behaves exactly as it did.
		detRows := m.detailRows(n, visible)
		m.detRows, m.rowCount = detRows, len(detRows)

		// Selection: follow the newest row until the user moves off it — unless
		// the wheel is driving the scroll (detailManual), in which case the offset is
		// honored as-is and the selection may sit off-screen (change 0041).
		if !m.detailManual {
			if m.followTail || m.rowSel >= len(detRows)-1 {
				m.rowSel = len(detRows) - 1
				m.followTail = true
			}
			if m.rowSel < 0 {
				m.rowSel = 0
			}
			// Keep the selection in the visible window.
			if m.rowSel < m.detailScroll {
				m.detailScroll = m.rowSel
			}
			if m.rowSel >= m.detailScroll+rows {
				m.detailScroll = m.rowSel - rows + 1
			}
		} else if m.rowSel < 0 {
			m.rowSel = 0
		}
		if m.rowSel >= len(detRows) {
			m.rowSel = len(detRows) - 1
		}
		// eventSel remains an index into `visible` — buildEventViewLines depends on
		// that — so re-derive it from the row under the cursor.
		m.syncEventSel(detRows, len(visible))
		all := m.renderDetailRows(n, visible, detRows, w)
		// Clamp the manual scroll to valid content bounds.
		if maxScroll := len(all) - rows; m.detailScroll > maxScroll {
			m.detailScroll = maxScroll
		}
		if m.detailScroll < 0 {
			m.detailScroll = 0
		}
		endIdx := m.detailScroll + rows
		if endIdx > len(all) {
			endIdx = len(all)
		}
		if m.detailScroll < len(all) {
			evtLines = all[m.detailScroll:endIdx]
		}
	}
	// "enter" is context-sensitive in the detail pane: on a turn header it
	// toggles the group collapsed/expanded (change 0066); on an event row it
	// drills in. The hint names the toggle so collapse is discoverable — the
	// old "enter expand" under-advertised it (expand-only).
	help := fitHelp(
		"j/k select  enter toggle/inspect  s original  b board  g/G first/last  esc back",
		"j/k · enter · s orig · b board · esc",
		w,
	)

	out := append(append([]string{}, headerLines...), rule)
	out = append(out, evtLines...)
	for len(out) < m.height-1 {
		out = append(out, "")
	}
	return append(out[:m.height-1], help)
}

// buildBlackboardLines renders the session blackboard snapshot in the detail
// pane (change 0023): each key (sorted) with a wrote-by indicator from the
// writer's label, followed by its JSON-rendered value. Every value/label byte is
// model/tool-controlled, so it runs through sanitizeDisplay and is hard-wrapped
// to the pane width before render — no line can exceed w or leak control bytes.
func (m *AgentsModel) buildBlackboardLines(w int) []string {
	header := lipgloss.NewStyle().Bold(true).Render(fitLine("Blackboard", w))
	rule := lipgloss.NewStyle().Foreground(colMuted).Render(strings.Repeat("─", w))
	help := fitHelp(
		"j/k scroll  n/p group  g top  b/esc back to tree",
		"j/k · n/p group · g · b/esc",
		w,
	)

	var body []string
	var groupStarts []int // body-line index of each writer-group header
	if m.blackboard == nil {
		body = []string{lipgloss.NewStyle().Foreground(colMuted).Render("No blackboard for this session.")}
	} else {
		snap := m.blackboard.Snapshot()
		if len(snap) == 0 {
			body = []string{lipgloss.NewStyle().Foreground(colMuted).Render("Blackboard is empty.")}
		} else {
			body, groupStarts = m.blackboardBody(snap, w)
		}
	}

	// Scroll window: header + rule occupy 2 rows, help occupies 1.
	rows := m.height - 3
	if rows < 1 {
		rows = 1
	}
	// Clamp to [0, max(0, len(body)-rows)] so the pane cannot over-scroll past
	// the last full window into a mostly-blank view (spec Decision 2; mirrors the
	// detail pane's len(all)-rows clamp).
	if maxScroll := len(body) - rows; m.bbScroll > maxScroll {
		m.bbScroll = maxScroll
	}
	if m.bbScroll < 0 {
		m.bbScroll = 0
	}
	end := m.bbScroll + rows
	if end > len(body) {
		end = len(body)
	}
	var window []string
	if m.bbScroll < len(body) {
		window = body[m.bbScroll:end]
	}

	// Sticky per-writer header: if the top of the window sits inside a group whose
	// header has scrolled above the window, pin that header as the first visible
	// line (one pinned header; no nested pinning). Drop the last content row so the
	// pinned header does not push the window over its row budget.
	if len(window) > 0 {
		if gs := stickyGroupStart(groupStarts, m.bbScroll); gs >= 0 && gs < m.bbScroll {
			pinned := stickyPinned(body[gs])
			trimmed := window
			if len(trimmed) >= rows && len(trimmed) > 1 {
				trimmed = trimmed[:len(trimmed)-1]
			}
			window = append([]string{pinned}, trimmed...)
		}
	}

	out := append([]string{header, rule}, window...)
	for len(out) < m.height-1 {
		out = append(out, "")
	}
	return append(out[:m.height-1], help)
}

// blackboardBody builds the grouped blackboard body lines and the body-line index
// of each writer-group header. Entries bucket by writer (WriterLabel, falling back
// to WriterID, then "(unknown)"); groups are ordered most-recent-first by the
// group's max WrittenAt; keys are sorted alphabetically within a group. Every
// model/tool-controlled field is sanitized and hard-wrapped to w so no line can
// exceed the pane width or leak control bytes.
//
// Turn awareness (change 0066) lives strictly INSIDE a writer group: when the
// root carries more than one turn mark, every group interleaves "── turn N ──"
// sub-dividers between its per-turn buckets — including a group that only wrote
// in a single turn, so its entries' turn-relative offsets are never unlabelled —
// and every entry's meta line carries a turn-relative offset. groupStarts still
// records only WRITER-group starts, so
// n/p navigation absorbs the extra divider lines automatically. With at most one
// turn mark on the root the output is byte-identical to the pre-0066 render.
func (m *AgentsModel) blackboardBody(snap map[string]agent.BlackboardEntry, w int) (body []string, groupStarts []int) {
	// Read the root node's turn marks through the single accessor both the render
	// path and blackboardGroupStarts reach (the latter calls this function), so
	// their line counts cannot diverge — the equality blackboardContentWidth's
	// comment protects.
	root := m.blackboardRootView()
	turnAware := len(root.Turns) > 1
	type group struct {
		name   string
		latest time.Time
		keys   []string
	}
	byName := map[string]*group{}
	for k, e := range snap {
		name := writerGroupName(e)
		g := byName[name]
		if g == nil {
			g = &group{name: name}
			byName[name] = g
		}
		g.keys = append(g.keys, k)
		if e.WrittenAt.After(g.latest) {
			g.latest = e.WrittenAt
		}
	}
	groups := make([]*group, 0, len(byName))
	for _, g := range byName {
		sort.Strings(g.keys)
		groups = append(groups, g)
	}
	// Most-recent-writer-first; ties broken by name for a stable, deterministic order.
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].latest.Equal(groups[j].latest) {
			return groups[i].name < groups[j].name
		}
		return groups[i].latest.After(groups[j].latest)
	})

	groupHeaderStyle := lipgloss.NewStyle().Foreground(colCyan).Bold(true)
	keyStyle := lipgloss.NewStyle().Foreground(colCyan).Bold(true)
	metaStyle := lipgloss.NewStyle().Foreground(colMuted)
	valStyle := lipgloss.NewStyle().Foreground(colNormal)
	sepStyle := lipgloss.NewStyle().Foreground(colMuted)

	for _, g := range groups {
		groupStarts = append(groupStarts, len(body))
		// Group header: "▌ <writer>" sanitized + fit to width.
		hdr := "▌ " + sanitizeDisplay(g.name)
		for _, hr := range wrapToWidth(hdr, w) {
			body = append(body, groupHeaderStyle.Render(hr))
		}
		// Bucket the group's keys by the turn each entry was written in, using THE
		// shared attribution rule (turnIndexFor) — never a second one. g.keys is
		// already alphabetical and the bucketing is stable, so ordering inside a
		// bucket is today's ordering; buckets themselves run turn-ascending.
		buckets := blackboardTurnBuckets(root, snap, g.keys, turnAware)
		for _, b := range buckets {
			// Sub-divider before each bucket whenever the board is turn-aware. It is
			// keyed on turnAware, not on len(buckets), because the meta lines are:
			// a group that wrote in exactly one turn still renders "· +12.3s", and
			// without its own divider nothing on screen says which turn that offset
			// is relative to — while the group next to it carries dividers. On a
			// board with at most one turn mark turnAware is false and no divider is
			// emitted, so the legacy bytes are unchanged.
			if turnAware {
				body = append(body, sepStyle.Render(fitLine(fmt.Sprintf("── turn %d ──", turnOrdinal(root, b.turnIdx)), w)))
			}
			for ki, k := range b.keys {
				e := snap[k]
				// Key line: accented key, muted "written by" meta.
				keyRows := wrapToWidth(sanitizeDisplay(k), w)
				for _, kr := range keyRows {
					body = append(body, keyStyle.Render(kr))
				}
				meta := "  ⟨written by " + sanitizeDisplay(e.WriterLabel) + "⟩"
				if turnAware {
					// Turn-relative, clamped by the shared helper: structurally never negative.
					meta = "  ⟨written by " + sanitizeDisplay(e.WriterLabel) + " · +" + turnOffsetLabel(root, e.WrittenAt) + "⟩"
				}
				for _, mr := range wrapToWidth(meta, w) {
					body = append(body, metaStyle.Render(mr))
				}
				// Value line(s): pretty JSON for objects/arrays (each already-indented
				// line wrapped individually), inline for scalars; normal contrast.
				for _, vl := range encodeBlackboardValueLines(e.Value, w) {
					body = append(body, valStyle.Render(vl))
				}
				// Separator between entries (muted rule) — suppressed before a
				// sub-divider, which already separates the buckets.
				if ki < len(b.keys)-1 {
					body = append(body, sepStyle.Render(fitLine(strings.Repeat("─", maxInt(1, w/3)), w)))
				}
			}
		}
		body = append(body, "") // blank line between groups
	}
	return body, groupStarts
}

// blackboardRootView is THE accessor the blackboard render path uses to reach the
// root node's turn marks. blackboardGroupStarts renders through blackboardBody,
// so both see exactly this view and their line counts cannot diverge. Returns a
// zero NodeView (no turns ⇒ legacy render) when there is no tree or no root.
func (m *AgentsModel) blackboardRootView() agent.NodeView {
	if m.tree == nil {
		return agent.NodeView{}
	}
	rootID := m.tree.RootID()
	if v, ok := m.nodeByID[rootID]; ok {
		return v
	}
	if n := m.tree.Node(rootID); n != nil {
		return n.Snapshot()
	}
	return agent.NodeView{}
}

// blackboardTurnBucket is one turn's slice of a writer group's keys.
type blackboardTurnBucket struct {
	turnIdx int // index into root.Turns; -1 on the legacy (turn-unaware) path
	keys    []string
}

// blackboardTurnBuckets splits one writer group's (already alphabetically sorted)
// keys into turn-ascending buckets. Attribution goes through turnIndexFor — the
// same rule the event pane uses — so an entry can never sit under a divider that
// disagrees with the offset printed on it. When the root is not multi-turn the
// whole group is a single bucket, which suppresses dividers and keeps the render
// byte-identical to the pre-0066 output.
func blackboardTurnBuckets(root agent.NodeView, snap map[string]agent.BlackboardEntry, keys []string, turnAware bool) []blackboardTurnBucket {
	if !turnAware {
		return []blackboardTurnBucket{{turnIdx: -1, keys: keys}}
	}
	byTurn := map[int][]string{}
	var order []int
	for _, k := range keys {
		ti := turnIndexFor(root, snap[k].WrittenAt)
		if _, seen := byTurn[ti]; !seen {
			order = append(order, ti)
		}
		byTurn[ti] = append(byTurn[ti], k)
	}
	sort.Ints(order)
	out := make([]blackboardTurnBucket, 0, len(order))
	for _, ti := range order {
		out = append(out, blackboardTurnBucket{turnIdx: ti, keys: byTurn[ti]})
	}
	return out
}

// writerGroupName picks the display bucket for an entry: WriterLabel, then
// WriterID, then "(unknown)".
func writerGroupName(e agent.BlackboardEntry) string {
	if s := strings.TrimSpace(e.WriterLabel); s != "" {
		return e.WriterLabel
	}
	if s := strings.TrimSpace(e.WriterID); s != "" {
		return e.WriterID
	}
	return "(unknown)"
}

// stickyGroupStart returns the body-line index of the header of the group that
// contains body line `top`, or -1 if none. groupStarts is ascending.
func stickyGroupStart(groupStarts []int, top int) int {
	gs := -1
	for _, s := range groupStarts {
		if s <= top {
			gs = s
		} else {
			break
		}
	}
	return gs
}

// stickyPinned re-renders a group header line as a pinned header (kept identical
// to the in-body header so the text is stable as it pins/unpins).
func stickyPinned(headerLine string) string { return headerLine }

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// encodeBlackboardValue JSON-encodes a stored value for display, falling back to
// a Go-formatted string if it is somehow not encodable.
func encodeBlackboardValue(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

// encodeBlackboardValueLines renders a stored value as display lines fit to width
// w. Scalars (string/number/bool/nil) render inline on a single indented line;
// object/array values pretty-print as 2-space-indented JSON with EACH already-
// indented line sanitized and wrapped individually (indentation preserved, the
// blob never re-flowed as one string — guardrail 2). Every returned line's display
// width is <= w.
func encodeBlackboardValueLines(v any, w int) []string {
	switch v.(type) {
	case map[string]any, []any:
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return wrapToWidth("  "+sanitizeDisplay(encodeBlackboardValue(v)), w)
		}
		var out []string
		for _, raw := range strings.Split(string(b), "\n") {
			// Preserve the JSON indentation; wrap this one already-indented line.
			for _, wl := range wrapToWidth("  "+sanitizeDisplay(raw), w) {
				out = append(out, wl)
			}
		}
		return out
	default:
		return wrapToWidth("  "+sanitizeDisplay(encodeBlackboardValue(v)), w)
	}
}

// wrapToWidth hard-wraps s (already sanitized) so no returned line's display
// width exceeds w. wrap.String breaks any run longer than w (word boundaries are
// ignored past the limit), so a single 500-char token still splits.
func wrapToWidth(s string, w int) []string {
	if w < 1 {
		w = 1
	}
	wrapped := wrap.String(wordwrap.String(s, w), w)
	return strings.Split(wrapped, "\n")
}

// turnStartFor resolves the start of the turn an event belongs to. Attribution
// is purely by timestamp: Turns is append-ordered, so the answer is the latest
// mark that had started by ts. It must NOT consult any event turn field
// (see change 0066 — that index counts tool-loop iterations, not conversational
// turns).
func turnStartFor(n agent.NodeView, ts time.Time) time.Time {
	i := turnIndexFor(n, ts)
	if i < 0 {
		return n.StartedAt
	}
	return n.Turns[i].StartedAt
}

// turnIndexFor is THE event→turn attribution rule, returning the index into
// n.Turns (-1 when the node has no marks — the legacy/child path). Both the
// offset formatter (via turnStartFor) and the detail pane's row grouping go
// through it, so a row can never disagree with the offset printed on it.
func turnIndexFor(n agent.NodeView, ts time.Time) int {
	if len(n.Turns) == 0 {
		return -1
	}
	for i := len(n.Turns) - 1; i >= 0; i-- {
		if !n.Turns[i].StartedAt.After(ts) {
			return i
		}
	}
	// The event predates turn 1; anchor it to the session's first turn so it
	// still renders a non-negative offset (and groups under turn 1's header).
	return 0
}

// detailRow is one rendered line of the detail pane's event list: either a turn
// group header or one event. Before change 0066 the pane emitted exactly one
// line per event, so the event index and the line index coincided; headers break
// that 1:1, and this row model is the single source of truth that replaces it.
type detailRow struct {
	header bool // true = turn header row
	turn   int  // conversational ordinal this row belongs to
	evtIdx int  // index into `visible`; -1 for a header row
}

// detailRows builds the detail pane's row model for one node.
//
// GUARD (the blast-radius container): a node with at most ONE turn mark — every
// child node, and a root before its second prompt — returns one non-header row
// per event, in order, so every downstream behavior is byte-identical to the
// pre-0066 pane. Only a genuinely multi-turn root takes the grouped path.
func (m *AgentsModel) detailRows(n agent.NodeView, visible []agent.AgentEvent) []detailRow {
	// Flat, one row per event — turn grouping now lives in the LEFT tree pane
	// (change turns-in-left-tree), so the detail pane is the pre-0066 per-node
	// transcript for every node, root included.
	rows := make([]detailRow, len(visible))
	for i := range visible {
		rows[i] = detailRow{turn: 1, evtIdx: i}
	}
	return rows
}

// turnOrdinal is the conversational ordinal a mark is keyed by. It prefers the
// mark's own Turn field and falls back to its position, so a mark built without
// an ordinal still gets a distinct, stable collapse key.
func turnOrdinal(n agent.NodeView, i int) int {
	if t := n.Turns[i].Turn; t > 0 {
		return t
	}
	return i + 1
}

// turnIsExpanded reports whether a turn group shows its events. An explicit
// operator toggle wins; absent one, the current (last) turn is expanded and
// every settled turn is collapsed.
func (m *AgentsModel) turnIsExpanded(turn int, isLast bool) bool {
	if v, ok := m.turnExpanded[turn]; ok {
		return v
	}
	return isLast
}

// toggleTurn flips a turn group's collapse state, recording the operator's
// explicit choice (which then survives later renders regardless of which turn
// is current).
func (m *AgentsModel) toggleTurn(turn int, isLast bool) {
	if m.turnExpanded == nil {
		m.turnExpanded = map[int]bool{}
	}
	m.turnExpanded[turn] = !m.turnIsExpanded(turn, isLast)
}

// syncEventSel keeps eventSel — still an index into `visible`, which
// buildEventViewLines depends on — in lockstep with the row cursor. On a header
// row the previous event selection is kept, only clamped back into range.
func (m *AgentsModel) syncEventSel(rows []detailRow, visibleLen int) {
	if m.rowSel >= 0 && m.rowSel < len(rows) {
		if r := rows[m.rowSel]; !r.header {
			m.eventSel = r.evtIdx
		}
	}
	if m.eventSel >= visibleLen {
		m.eventSel = visibleLen - 1
	}
	if m.eventSel < 0 {
		m.eventSel = 0
	}
}

// turnPromptPreview renders a turn's raw prompt as a single-line, quoted
// preview of at most budget cells. Newlines are flattened to spaces (a header is
// ONE row — a surviving newline would silently split it and desync the pane's
// line accounting) and every other control byte is neutralized by
// sanitizeDisplay.
func turnPromptPreview(raw string, budget int) string {
	s := sanitizeDisplay(raw)
	s = strings.Join(strings.Fields(strings.ReplaceAll(s, "\n", " ")), " ")
	if s == "" {
		return "(no prompt)"
	}
	return `"` + truncateCells(s, maxInt(1, budget-2)) + `"`
}

// truncateCells caps s at n DISPLAY CELLS — the ellipsis included — where
// renderer.go's truncate caps BYTES. Fixed-width panes budget in cells
// (lipgloss.Width), so handing a cell budget to a byte truncator is wrong in
// both directions: ASCII overflows by the unreserved ellipsis (and fitLine then
// absorbs the excess by truncating from the RIGHT, eating the row's
// duration/event-count suffix), while CJK/emoji under-fill by roughly two
// thirds. Wide runes are never split, so the result may land one cell short.
// A non-positive budget has no room for content or an ellipsis, so it yields "".
func truncateCells(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= n {
		return s
	}
	if n <= 1 {
		return "…"
	}
	limit := n - 1 // reserve the ellipsis
	w := 0
	for i, r := range s {
		rw := lipgloss.Width(string(r))
		if w+rw > limit {
			return s[:i] + "…"
		}
		w += rw
	}
	return s + "…"
}

// eventOffset formats an event's turn-relative offset. It clamps at zero: the
// defect this replaces was a negative number, so the renderer is structurally
// unable to emit one.
func eventOffset(n agent.NodeView, ts time.Time) string {
	d := ts.Sub(turnStartFor(n, ts))
	if d < 0 {
		d = 0
	}
	return fmt.Sprintf("%05.1fs", d.Seconds())
}

// turnOffsetLabel is eventOffset's INLINE form: the same clamped, turn-relative
// duration, without the zero padding. eventOffset pads to a fixed width because
// the detail pane renders it inside a "[…]" column that must stay aligned; the
// blackboard's "⟨written by alice · +12.3s⟩" is prose, where a padded "+012.3s"
// reads as a rendering fault rather than a duration (found at change 0066's live
// verification). Both share turnStartFor, so attribution cannot diverge.
func turnOffsetLabel(n agent.NodeView, ts time.Time) string {
	d := ts.Sub(turnStartFor(n, ts))
	if d < 0 {
		d = 0
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}

// buildEventViewLines renders one event's COMPLETE content, word-wrapped and
// scrollable — the drill-in for output that every list view truncates.
func (m *AgentsModel) buildEventViewLines(n agent.NodeView, visible []agent.AgentEvent, w int) []string {
	if m.eventSel >= len(visible) {
		m.eventSel = len(visible) - 1
	}
	if m.eventSel < 0 {
		m.eventSel = 0
	}
	evt := visible[m.eventSel]

	ts := " 0.0s"
	if !n.StartedAt.IsZero() {
		ts = eventOffset(n, evt.TS)
	}
	title := fmt.Sprintf("event %d/%d  [%s]  %s  %s", m.eventSel+1, len(visible), ts,
		strings.TrimSpace(detailKind(evt.Kind)), evt.Name)
	header := fitLine(lipgloss.NewStyle().Foreground(colNormal).Bold(true).Render(title), w)
	rule := lipgloss.NewStyle().Foreground(colMuted).Render(strings.Repeat("─", w))

	wrapW := w - 1
	if wrapW < 8 {
		wrapW = 8
	}
	// wordwrap breaks at spaces; wrap hard-breaks anything still over width
	// (long tokens, minified JSON) so no line can overflow the pane and
	// desync the terminal's row layout.
	body := wrap.String(wordwrap.String(sanitizeDisplay(eventFullContent(evt)), wrapW), wrapW)
	lines := strings.Split(body, "\n")
	for i, l := range lines {
		lines[i] = fitLine(l, w)
	}

	rows := m.height - 3
	if rows < 1 {
		rows = 1
	}
	maxScroll := len(lines) - rows
	if maxScroll < 0 {
		maxScroll = 0
	}
	if m.eventScroll > maxScroll {
		m.eventScroll = maxScroll
	}
	if m.eventScroll < 0 {
		m.eventScroll = 0
	}
	end := m.eventScroll + rows
	if end > len(lines) {
		end = len(lines)
	}
	window := lines[m.eventScroll:end]

	help := fitHelp(
		"j/k scroll  g/G top/bottom  esc back to events",
		"j/k · g/G · esc back",
		w,
	)
	out := append([]string{header, rule}, window...)
	for len(out) < m.height-1 {
		out = append(out, "")
	}
	return append(out[:m.height-1], help)
}

// eventFullContent returns an event's complete payload, untruncated.
func eventFullContent(evt agent.AgentEvent) string {
	switch evt.Kind {
	case agent.KindAssistant:
		if t, ok := evt.Payload["text"].(string); ok {
			return t
		}
	case agent.KindToolCall:
		args, _ := evt.Payload["args"].(string)
		return evt.Name + "(" + args + ")"
	case agent.KindToolResult:
		if o, ok := evt.Payload["output"].(string); ok {
			return o
		}
	case agent.KindError:
		if e, ok := evt.Payload["error"].(string); ok {
			return e
		}
	case agent.KindDone:
		if r, ok := evt.Payload["result"].(string); ok {
			return r
		}
	}
	if evt.Name != "" {
		return evt.Name
	}
	return "(no content)"
}

func (m *AgentsModel) renderDetailHeader(n agent.NodeView, w int) string {
	label := sanitizeDisplay(n.Label)
	// When the selection is a turn header (the root scoped to a turn), name the
	// turn so the operator knows the transcript below is that turn's slice.
	if tf := m.selectedTurnFilter(); tf > 0 {
		label += " · turn " + fmt.Sprintf("%d", tf)
	}

	glyph := glyphStyle(n.Status).Render(glyphForStatus(n.Status))
	elapsed := nodeElapsed(n)
	tokens := ""
	if n.TokensIn > 0 || n.TokensOut > 0 {
		tokens = " ↑" + formatTokens(n.TokensIn) + " ↓" + formatTokens(n.TokensOut)
	}
	rightPlain := glyphForStatus(n.Status) + " " + elapsed + tokens
	lw := lipgloss.Width(label)
	rw := lipgloss.Width(rightPlain)
	space := w - lw - rw - 2
	if space < 1 {
		space = 1
	}

	rightS := glyph + lipgloss.NewStyle().Foreground(colMuted).Render(" "+elapsed+tokens)
	top := fitLine(
		lipgloss.NewStyle().Foreground(colNormal).Render(label)+
			strings.Repeat(" ", space)+rightS,
		w,
	)
	// Compaction indicator (change 0030): a compact segments line under the
	// header when the (root) node has archived segments. Returned as a second
	// line joined by "\n"; buildDetailLines splits it back out.
	if ind := m.segmentIndicatorLine(n); ind != "" {
		return top + "\n" + fitLine(lipgloss.NewStyle().Foreground(colMuted).Render(ind), w)
	}
	return top
}

// renderDetailRows renders the detail pane's row model: one line per row,
// headers included, with the row at m.rowSel highlighted. For a single-turn
// root or a child node the row model is 1:1 with the events (see detailRows'
// guard), so this emits exactly what the pre-0066 flat event renderer did —
// byte for byte. That renderer is no longer on the render path; it survives as
// legacyEventLinesGolden in turn_groups_test.go, whose only purpose is to pin
// this equality.
// renderDetailRows renders the detail pane's flat row model: one line per event.
// Turn grouping now lives in the LEFT tree (change turns-in-left-tree), so this
// pane is the pre-0066 per-node transcript with per-turn-relative offsets.
func (m *AgentsModel) renderDetailRows(n agent.NodeView, events []agent.AgentEvent, rows []detailRow, w int) []string {
	lines := make([]string, 0, len(rows))
	for i, r := range rows {
		lines = append(lines, m.renderEventRow(n, events[r.evtIdx], i == m.rowSel, w))
	}
	return lines
}

// renderEventRow renders one event as a single selectable row of exactly w
// cells.
func (m *AgentsModel) renderEventRow(n agent.NodeView, evt agent.AgentEvent, selected bool, w int) string {
	ts := " 0.0s"
	if !n.StartedAt.IsZero() {
		ts = eventOffset(n, evt.TS)
	}

	arrow := detailArrow(evt.Kind)
	kind := detailKind(evt.Kind) // 9 chars, space-padded

	prefixPlain := "[" + ts + "] " + arrow + " " + kind + "  "
	maxContent := w - lipgloss.Width(prefixPlain)
	if maxContent < 4 {
		maxContent = 4
	}

	content := sanitizeDisplay(detailContent(evt, maxContent))

	if selected {
		// Selected row: plain reverse video so it reads as a cursor.
		return fitLine(lipgloss.NewStyle().Reverse(true).Render(prefixPlain+content), w)
	}

	prefix := lipgloss.NewStyle().Foreground(colMuted).Render(prefixPlain)
	var contentS string
	switch evt.Kind {
	case agent.KindAssistant:
		contentS = lipgloss.NewStyle().Foreground(colNormal).Render(content)
	case agent.KindToolCall:
		contentS = lipgloss.NewStyle().Foreground(colCyan).Render(content)
	case agent.KindToolResult:
		contentS = lipgloss.NewStyle().Foreground(colMuted).Render(content)
	case agent.KindError:
		contentS = lipgloss.NewStyle().Foreground(colRed).Render(content)
	default:
		contentS = lipgloss.NewStyle().Foreground(colMuted).Render(content)
	}
	return fitLine(prefix+contentS, w)
}

// ─── snapshot helpers ─────────────────────────────────────────────────────────

func (m *AgentsModel) refreshSnapshot() {
	m.nodes = m.tree.SnapshotAll()
	m.nodeByID = make(map[string]agent.NodeView, len(m.nodes))
	for _, n := range m.nodes {
		m.nodeByID[n.ID] = n
	}
	m.lastChildOf = make(map[string]string, len(m.nodes))
	for _, n := range m.nodes {
		if n.ParentID != "" {
			m.lastChildOf[n.ParentID] = n.ID
		}
	}
	// Rebuild the tree row model here too and clamp the cursor against ITS length,
	// not len(m.nodes). m.selected indexes the row model (turn headers + nodes),
	// which is larger than the node list whenever there are turn headers. Clamping
	// to len(m.nodes)-1 — as this did before turns-in-left-tree — yanked the cursor
	// back up on every refresh (each tree update / 250ms tick) once it moved past
	// the node count, so on a fresh turn the down arrow "snapped to the top" until
	// enough nodes appeared to grow len(m.nodes). Keeping treeRows fresh here also
	// means handleTreeKey (which reads m.treeRowCount) sees the current count even
	// before the next render.
	m.treeRows = m.buildTreeRowModel()
	m.treeRowCount = len(m.treeRows)
	if m.selected >= m.treeRowCount && m.treeRowCount > 0 {
		m.selected = m.treeRowCount - 1
	}
}

// ─── event detail helpers ─────────────────────────────────────────────────────

func detailKind(k agent.EventKind) string {
	switch k {
	case agent.KindAssistant:
		return "assistant"
	case agent.KindToolCall:
		return "tool_call"
	case agent.KindToolResult:
		return "result   "
	case agent.KindError:
		return "error    "
	case agent.KindDone:
		return "done     "
	default:
		return "event    "
	}
}

func detailArrow(k agent.EventKind) string {
	switch k {
	case agent.KindToolResult, agent.KindTokens:
		return "◂"
	default:
		return "▸"
	}
}

func detailContent(evt agent.AgentEvent, maxW int) string {
	switch evt.Kind {
	case agent.KindAssistant:
		if t, ok := evt.Payload["text"].(string); ok {
			return truncate(firstLine(t), maxW)
		}
	case agent.KindToolCall:
		args := ""
		if a, ok := evt.Payload["args"].(string); ok {
			args = a
		}
		return truncate(evt.Name+"("+args+")", maxW)
	case agent.KindToolResult:
		if o, ok := evt.Payload["output"].(string); ok {
			return truncate(firstLine(o), maxW)
		}
	case agent.KindTokens:
		in, out := 0, 0
		if v, ok := evt.Payload["in"].(int); ok {
			in = v
		}
		if v, ok := evt.Payload["out"].(int); ok {
			out = v
		}
		return "↑" + formatTokens(in) + " ↓" + formatTokens(out)
	case agent.KindError:
		if e, ok := evt.Payload["error"].(string); ok {
			return truncate(e, maxW)
		}
	case agent.KindDone:
		if r, ok := evt.Payload["result"].(string); ok {
			return truncate(firstLine(r), maxW)
		}
	}
	if evt.Name != "" {
		return truncate(evt.Name, maxW)
	}
	return ""
}

// firstLine returns the first non-empty line of s.
func firstLine(s string) string {
	for _, l := range strings.SplitN(s, "\n", 5) {
		l = strings.TrimSpace(l)
		if l != "" {
			return l
		}
	}
	return s
}

// ─── status/glyph helpers ─────────────────────────────────────────────────────

func glyphForStatus(s agent.NodeStatus) string {
	switch s {
	case agent.StatusRunning:
		return "●"
	case agent.StatusPending:
		return "◐"
	case agent.StatusDone:
		return "✓"
	case agent.StatusError:
		return "✕"
	case agent.StatusCancelled:
		return "○"
	default:
		return "?"
	}
}

func glyphStyle(s agent.NodeStatus) lipgloss.Style {
	switch s {
	case agent.StatusRunning:
		return lipgloss.NewStyle().Foreground(colAmber)
	case agent.StatusDone:
		return lipgloss.NewStyle().Foreground(colGreen)
	case agent.StatusError:
		return lipgloss.NewStyle().Foreground(colRed)
	default:
		return lipgloss.NewStyle().Foreground(colMuted)
	}
}

func nodeElapsed(n agent.NodeView) string {
	if len(n.Turns) > 0 {
		last := n.Turns[len(n.Turns)-1]
		d := time.Since(last.StartedAt)
		if !last.EndedAt.IsZero() {
			d = last.EndedAt.Sub(last.StartedAt)
		}
		return formatNodeElapsed(d)
	}
	if n.StartedAt.IsZero() {
		return "–"
	}
	d := time.Since(n.StartedAt)
	if !n.EndedAt.IsZero() {
		d = n.EndedAt.Sub(n.StartedAt)
	}
	return formatNodeElapsed(d)
}

// formatNodeElapsed renders a duration as seconds under a minute, else "%dm%.0fs".
func formatNodeElapsed(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.0fs", d.Seconds())
	}
	return fmt.Sprintf("%dm%.0fs", int(d.Minutes()), d.Seconds()-float64(int(d.Minutes()))*60)
}

// fitLine truncates s to w visible cells and then space-pads to exactly w.
// This guarantees every column line is exactly w cells wide so that manual
// column joining produces correct output without overflow or misalignment.
// fitHelp renders a pane's help bar to fit width w without truncation jamming
// against the divider: it prefers the full text, falls back to a compact form
// when the pane is too narrow (the common case for the 40% tree column), and
// only truncates if even the compact form overflows. Returned styled and padded
// to exactly w so the divider column stays aligned. Without this a long help
// string is hard-truncated by fitLine mid-word, dropping keys (e.g. "esc exit")
// and butting the text straight up against the divider.
func fitHelp(full, compact string, w int) string {
	if lipgloss.Width(full) <= w {
		return fitLine(treeHelpStyle.Render(full), w)
	}
	if lipgloss.Width(compact) <= w {
		return fitLine(treeHelpStyle.Render(compact), w)
	}
	return fitLine(treeHelpStyle.Render(compact), w)
}

func fitLine(s string, w int) string {
	vw := lipgloss.Width(s)
	switch {
	case vw > w:
		// lipgloss MaxWidth truncates ANSI-aware to w cells.
		return lipgloss.NewStyle().MaxWidth(w).Render(s)
	case vw < w:
		return s + strings.Repeat(" ", w-vw)
	default:
		return s
	}
}

// treeHelpStyle is the style for the help bar at the bottom of each pane.
var treeHelpStyle = lipgloss.NewStyle().Foreground(colMuted)
