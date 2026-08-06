package tui

import (
	"fmt"
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
//	Tree:   j/k navigate  g/G first/last  enter/tab detail  x cancel  esc exit
//	Detail: j/k scroll    g/G top/bottom  q/esc/tab back-to-tree
type AgentsModel struct {
	tree         *agent.AgentTree
	nodes        []agent.NodeView // depth-first snapshot, refreshed on tree update
	nodeByID     map[string]agent.NodeView
	lastChildOf  map[string]string // parentID → last child's ID
	selected     int
	treeScroll   int // first visible row of the tree pane
	inDetail     bool
	detailScroll int  // first visible event row in the detail list
	followTail   bool // selection sticks to the newest event until user moves up
	eventSel     int  // selected event (index into displayEvents)
	eventCount   int  // len(displayEvents) as of last render, for key clamping
	inEventView  bool // full-content view of the selected event
	eventScroll  int  // scroll offset within the expanded event
	width        int
	height       int
	// rateGate is the session's shared rate-gate bucket, or nil (change 0036). Its
	// live utilization joins the scheduler summary block at the top of the tree
	// pane so the overlay shows the throughput brake alongside the pool counters.
	rateGate *ratelimit.Bucket
}

// NewAgentsModel creates an AgentsModel backed by the given tree, with an
// optional rate-gate bucket for the scheduler summary block (nil = no gate).
func NewAgentsModel(t *agent.AgentTree, gate *ratelimit.Bucket) *AgentsModel {
	m := &AgentsModel{tree: t, rateGate: gate, followTail: true}
	m.refreshSnapshot()
	return m
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
	header := m.schedulerHeaderLines(m.width)
	h := m.height - len(header)
	if h < 1 {
		h = 1
	}
	fullHeight := m.height
	m.height = h
	treeLines := m.buildTreeLines(treeW)
	detailLines := m.buildDetailLines(detailW)
	m.height = fullHeight
	for len(treeLines) < h {
		treeLines = append(treeLines, strings.Repeat(" ", treeW))
	}
	for len(detailLines) < h {
		detailLines = append(detailLines, strings.Repeat(" ", detailW))
	}

	divChar := lipgloss.NewStyle().Foreground(colMuted).Render("│")
	lines := make([]string, 0, len(header)+h)
	lines = append(lines, header...)
	for i := 0; i < h; i++ {
		lines = append(lines, fitLine(treeLines[i], treeW)+divChar+fitLine(detailLines[i], detailW))
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

// ─── key handlers ─────────────────────────────────────────────────────────────

func (m *AgentsModel) handleTreeKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	n := len(m.nodes)
	switch msg.String() {
	case "j", "down":
		if m.selected < n-1 {
			m.selected++
		}
	case "k", "up":
		if m.selected > 0 {
			m.selected--
		}
	case "g":
		m.selected = 0
	case "G":
		if n > 0 {
			m.selected = n - 1
		}
	case "enter", "tab":
		m.inDetail = true
		m.detailScroll = 0
		m.eventSel = 0
		m.followTail = true // land on the newest event of the chosen node
	case "x":
		if n > 0 && m.selected < n {
			m.tree.CancelNode(m.nodes[m.selected].ID)
		}
	case "q", "esc":
		return m, func() tea.Msg { return agentsExitMsg{} }
	}
	return m, nil
}

func (m *AgentsModel) handleDetailKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "j", "down":
		if m.eventSel < m.eventCount-1 {
			m.eventSel++
		}
		m.followTail = m.eventSel >= m.eventCount-1
	case "k", "up":
		if m.eventSel > 0 {
			m.eventSel--
		}
		m.followTail = false
	case "g":
		m.eventSel = 0
		m.followTail = false
	case "G":
		m.followTail = true
	case "enter":
		if m.eventCount > 0 {
			m.inEventView = true
			m.eventScroll = 0
		}
	case "q", "esc", "tab":
		m.inDetail = false
		m.detailScroll = 0
		m.followTail = true
	}
	return m, nil
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

// handleMouse: the wheel follows the active pane — node selection in the
// tree, event selection in the detail list, content scroll in an expanded
// event.
func (m *AgentsModel) handleMouse(msg tea.MouseMsg) {
	up := msg.Button == tea.MouseButtonWheelUp
	down := msg.Button == tea.MouseButtonWheelDown
	if !up && !down {
		return
	}
	switch {
	case m.inEventView:
		if up {
			m.eventScroll -= 3
			if m.eventScroll < 0 {
				m.eventScroll = 0
			}
		} else {
			m.eventScroll += 3 // clamped at render time
		}
	case m.inDetail:
		if up {
			if m.eventSel > 0 {
				m.eventSel--
			}
			m.followTail = false
		} else {
			if m.eventSel < m.eventCount-1 {
				m.eventSel++
			}
			m.followTail = m.eventSel >= m.eventCount-1
		}
	default: // tree pane
		if up && m.selected > 0 {
			m.selected--
		}
		if down && m.selected < len(m.nodes)-1 {
			m.selected++
		}
	}
}

// ─── tree panel ───────────────────────────────────────────────────────────────

// buildTreeLines returns m.height lines for the tree panel (last line = help
// bar), scrolled so the selected node is always visible — big trees overflow
// the pane and must window, not clip.
func (m *AgentsModel) buildTreeLines(w int) []string {
	rows := m.renderTreeRows(w)
	help := treeHelpStyle.Render("j/k select  enter inspect  x cancel  esc exit")

	visRows := m.height - 1
	if visRows < 1 {
		visRows = 1
	}
	// Keep the selection inside the window.
	if m.selected < m.treeScroll {
		m.treeScroll = m.selected
	}
	if m.selected >= m.treeScroll+visRows {
		m.treeScroll = m.selected - visRows + 1
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

func (m *AgentsModel) renderTreeRows(w int) []string {
	if len(m.nodes) == 0 {
		return []string{lipgloss.NewStyle().Foreground(colMuted).Render("No agents yet.")}
	}
	rows := make([]string, 0, len(m.nodes))
	for i, n := range m.nodes {
		rows = append(rows, m.renderNodeRow(n, i == m.selected, w))
	}
	return rows
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
	if len(m.nodes) == 0 || m.selected >= len(m.nodes) {
		return []string{lipgloss.NewStyle().Foreground(colMuted).Render("Select a node to inspect.")}
	}

	n := m.nodes[m.selected]
	// Events aren't carried in the snapshot (they can be large); take a
	// race-safe copy from the live node only for the selected one.
	var events []agent.AgentEvent
	if live := m.tree.Node(n.ID); live != nil {
		events = live.CopyEvents()
	}
	visible := displayEvents(events)
	m.eventCount = len(visible)

	if m.inEventView && len(visible) > 0 {
		return m.buildEventViewLines(n, visible, w)
	}
	m.inEventView = false

	header := m.renderDetailHeader(n, w)
	rule := lipgloss.NewStyle().Foreground(colMuted).Render(strings.Repeat("─", w))

	rows := m.height - 3 // header + rule + help
	if rows < 1 {
		rows = 1
	}

	var evtLines []string
	if len(visible) == 0 {
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
		// Selection: follow the newest event until the user moves off it.
		if m.followTail || m.eventSel >= len(visible)-1 {
			m.eventSel = len(visible) - 1
			m.followTail = true
		}
		if m.eventSel < 0 {
			m.eventSel = 0
		}
		// Keep the selection in the visible window.
		if m.eventSel < m.detailScroll {
			m.detailScroll = m.eventSel
		}
		if m.eventSel >= m.detailScroll+rows {
			m.detailScroll = m.eventSel - rows + 1
		}
		all := m.renderEventLines(n, visible, w)
		endIdx := m.detailScroll + rows
		if endIdx > len(all) {
			endIdx = len(all)
		}
		if m.detailScroll < len(all) {
			evtLines = all[m.detailScroll:endIdx]
		}
	}
	help := treeHelpStyle.Render("j/k select  enter expand  g/G first/last  esc back")

	out := append([]string{header, rule}, evtLines...)
	for len(out) < m.height-1 {
		out = append(out, "")
	}
	return append(out[:m.height-1], help)
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
		ts = fmt.Sprintf("%05.1fs", evt.TS.Sub(n.StartedAt).Seconds())
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

	help := treeHelpStyle.Render("j/k scroll  g/G top/bottom  esc back to events")
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
	return fitLine(
		lipgloss.NewStyle().Foreground(colNormal).Render(label)+
			strings.Repeat(" ", space)+rightS,
		w,
	)
}

// renderEventLines renders one selectable row per (pre-filtered) event; the
// row at m.eventSel is highlighted.
func (m *AgentsModel) renderEventLines(n agent.NodeView, events []agent.AgentEvent, w int) []string {
	var lines []string
	for i, evt := range events {
		ts := " 0.0s"
		if !n.StartedAt.IsZero() {
			ts = fmt.Sprintf("%05.1fs", evt.TS.Sub(n.StartedAt).Seconds())
		}

		arrow := detailArrow(evt.Kind)
		kind := detailKind(evt.Kind) // 9 chars, space-padded

		prefixPlain := "[" + ts + "] " + arrow + " " + kind + "  "
		maxContent := w - lipgloss.Width(prefixPlain)
		if maxContent < 4 {
			maxContent = 4
		}

		content := sanitizeDisplay(detailContent(evt, maxContent))

		if i == m.eventSel {
			// Selected row: plain reverse video so it reads as a cursor.
			lines = append(lines, fitLine(lipgloss.NewStyle().Reverse(true).Render(prefixPlain+content), w))
			continue
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
		lines = append(lines, fitLine(prefix+contentS, w))
	}
	return lines
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
	if m.selected >= len(m.nodes) && len(m.nodes) > 0 {
		m.selected = len(m.nodes) - 1
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
	if n.StartedAt.IsZero() {
		return "–"
	}
	d := time.Since(n.StartedAt)
	if !n.EndedAt.IsZero() {
		d = n.EndedAt.Sub(n.StartedAt)
	}
	if d < time.Minute {
		return fmt.Sprintf("%.0fs", d.Seconds())
	}
	return fmt.Sprintf("%dm%.0fs", int(d.Minutes()), d.Seconds()-float64(int(d.Minutes()))*60)
}

// fitLine truncates s to w visible cells and then space-pads to exactly w.
// This guarantees every column line is exactly w cells wide so that manual
// column joining produces correct output without overflow or misalignment.
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
