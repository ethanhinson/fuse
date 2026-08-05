package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/ethanhinson/fuse/internal/agent"
)

// agentsExitMsg is returned to the parent ShellModel when AgentsModel exits.
type agentsExitMsg struct{}

// treeUpdateMsg signals that an agent tree update has arrived.
type treeUpdateMsg struct{ nodeID string }

// waitForTreeUpdate blocks on the tree's update channel and delivers it as a
// treeUpdateMsg. Re-armed after every received event.
func waitForTreeUpdate(t *agent.AgentTree) tea.Cmd {
	return func() tea.Msg {
		u := <-t.Updates()
		return treeUpdateMsg{nodeID: u.NodeID}
	}
}

// AgentsModel renders the spatial agent tree overlay (Tab / /agents).
// Layout: 40% tree panel | 1-char divider | 60% detail panel.
//
// Key bindings per §4.2:
//
//	Tree:   j/k navigate  g/G first/last  enter/tab detail  x cancel  esc exit
//	Detail: j/k scroll    g/G top/bottom  q/esc/tab back-to-tree
type AgentsModel struct {
	tree         *agent.AgentTree
	nodes        []*agent.AgentNode // depth-first snapshot, refreshed on tree update
	nodeByID     map[string]*agent.AgentNode
	lastChildOf  map[string]string // parentID → last child's ID
	selected     int
	inDetail     bool
	detailScroll int
	width        int
	height       int
}

// NewAgentsModel creates an AgentsModel backed by the given tree.
func NewAgentsModel(t *agent.AgentTree) *AgentsModel {
	m := &AgentsModel{tree: t}
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
	case tea.KeyMsg:
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

	treeLines := m.buildTreeLines(treeW)
	detailLines := m.buildDetailLines(detailW)

	h := m.height
	for len(treeLines) < h {
		treeLines = append(treeLines, strings.Repeat(" ", treeW))
	}
	for len(detailLines) < h {
		detailLines = append(detailLines, strings.Repeat(" ", detailW))
	}

	divChar := lipgloss.NewStyle().Foreground(colMuted).Render("│")
	var b strings.Builder
	for i := 0; i < h; i++ {
		b.WriteString(fitLine(treeLines[i], treeW))
		b.WriteString(divChar)
		b.WriteString(fitLine(detailLines[i], detailW))
		if i < h-1 {
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
		m.detailScroll++
	case "k", "up":
		if m.detailScroll > 0 {
			m.detailScroll--
		}
	case "g":
		m.detailScroll = 0
	case "G":
		m.detailScroll = 9999
	case "q", "esc", "tab":
		m.inDetail = false
		m.detailScroll = 0
	}
	return m, nil
}

// ─── tree panel ───────────────────────────────────────────────────────────────

// buildTreeLines returns m.height lines for the tree panel (last line = help bar).
func (m *AgentsModel) buildTreeLines(w int) []string {
	rows := m.renderTreeRows(w)
	help := treeHelpStyle.Render("j/k select  enter inspect  x cancel  esc exit")

	for len(rows) < m.height-1 {
		rows = append(rows, "")
	}
	return append(rows[:m.height-1], help)
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
func (m *AgentsModel) renderNodeRow(n *agent.AgentNode, selected bool, w int) string {
	edge := m.edgePrefix(n)
	cloud := ""
	if n.RemoteExec {
		cloud = "☁ "
	}

	glyph := glyphForStatus(n.Status)
	elapsed := nodeElapsed(n)
	tokens := ""
	if n.TokensIn > 0 || n.TokensOut > 0 {
		tokens = " ↑" + formatTokens(n.TokensIn) + " ↓" + formatTokens(n.TokensOut)
	}
	right := glyph + " " + elapsed + tokens

	prefixPlain := edge + cloud + n.Label
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
	labelS := lipgloss.NewStyle().Foreground(colNormal).Render(n.Label)
	dotsS := lipgloss.NewStyle().Foreground(colMuted).Render("  " + dots + "  ")
	glyphS := glyphStyle(n.Status).Render(glyph)
	rightS := glyphS + lipgloss.NewStyle().Foreground(colMuted).Render(" "+elapsed+tokens)

	return fitLine(edgeS+cloudS+labelS+dotsS+rightS, w)
}

// edgePrefix builds the box-drawing edge for n using isLastChild lookups.
func (m *AgentsModel) edgePrefix(n *agent.AgentNode) string {
	if n.Depth == 0 {
		return ""
	}
	var chain []*agent.AgentNode
	cur := m.nodeByID[n.ParentID]
	for cur != nil && cur.Depth > 0 {
		chain = append([]*agent.AgentNode{cur}, chain...)
		cur = m.nodeByID[cur.ParentID]
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

// buildDetailLines returns m.height lines for the detail panel.
func (m *AgentsModel) buildDetailLines(w int) []string {
	if len(m.nodes) == 0 || m.selected >= len(m.nodes) {
		return []string{lipgloss.NewStyle().Foreground(colMuted).Render("Select a node to inspect.")}
	}

	n := m.nodes[m.selected]
	events := n.CopyEvents()

	header := m.renderDetailHeader(n, w)
	rule := lipgloss.NewStyle().Foreground(colMuted).Render(strings.Repeat("─", w))
	evtLines := m.renderEventLines(n, events, w)
	help := treeHelpStyle.Render("j/k scroll  g/G top/bottom  esc back")

	all := append([]string{header, rule}, evtLines...)

	// Clamp scroll.
	maxScroll := len(all) - (m.height - 2)
	if maxScroll < 0 {
		maxScroll = 0
	}
	scroll := m.detailScroll
	if scroll > maxScroll {
		scroll = maxScroll
	}
	if scroll > 0 && scroll < len(all) {
		all = all[scroll:]
	}

	for len(all) < m.height-1 {
		all = append(all, "")
	}
	return append(all[:m.height-1], help)
}

func (m *AgentsModel) renderDetailHeader(n *agent.AgentNode, w int) string {
	label := n.Label
	if n.RemoteExec {
		label += " ☁"
	}

	glyph := glyphStyle(n.Status).Render(glyphForStatus(n.Status))
	elapsed := nodeElapsed(n)
	tokens := ""
	if n.TokensIn > 0 || n.TokensOut > 0 {
		tokens = " ↑" + formatTokens(n.TokensIn) + " ↓" + formatTokens(n.TokensOut)
	}
	cost := ""
	if n.CostUSD > 0 {
		cost = fmt.Sprintf(" $%.3f", n.CostUSD)
	}

	rightPlain := glyphForStatus(n.Status) + " " + elapsed + tokens + cost
	lw := lipgloss.Width(label)
	rw := lipgloss.Width(rightPlain)
	space := w - lw - rw - 2
	if space < 1 {
		space = 1
	}

	rightS := glyph + lipgloss.NewStyle().Foreground(colMuted).Render(" "+elapsed+tokens+cost)
	return fitLine(
		lipgloss.NewStyle().Foreground(colNormal).Render(label)+
			strings.Repeat(" ", space)+rightS,
		w,
	)
}

func (m *AgentsModel) renderEventLines(n *agent.AgentNode, events []agent.AgentEvent, w int) []string {
	var lines []string
	for _, evt := range events {
		if evt.Kind == agent.KindTokens {
			continue
		}

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

		content := detailContent(evt, maxContent)
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
	m.nodes = m.tree.Nodes()
	m.nodeByID = make(map[string]*agent.AgentNode, len(m.nodes))
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
	case agent.KindSpawned:
		return "spawned  "
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

func nodeElapsed(n *agent.AgentNode) string {
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
