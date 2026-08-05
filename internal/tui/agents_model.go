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

// AgentsModel is the bubbletea model for the /agents view.
// It shows a 40/60 tree/detail split with keyboard navigation.
type AgentsModel struct {
	tree         *agent.AgentTree
	nodes        []*agent.AgentNode
	selected     int
	detail       bool
	detailScroll int
	width        int
	height       int
}

// NewAgentsModel creates an AgentsModel backed by the given tree.
func NewAgentsModel(t *agent.AgentTree) *AgentsModel {
	m := &AgentsModel{tree: t}
	m.nodes = t.Nodes()
	return m
}

// Init is a no-op; the parent ShellModel owns the tree-update subscription.
func (m *AgentsModel) Init() tea.Cmd { return nil }

// waitForTreeUpdate blocks on the tree's update channel and delivers it as a
// treeUpdateMsg. Re-armed after every received event.
func waitForTreeUpdate(t *agent.AgentTree) tea.Cmd {
	return func() tea.Msg {
		u := <-t.Updates()
		return treeUpdateMsg{nodeID: u.NodeID}
	}
}

// Update handles key events and tree updates.
func (m *AgentsModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case treeUpdateMsg:
		m.nodes = m.tree.Nodes()
		return m, nil

	case tea.KeyMsg:
		if m.detail {
			return m.handleDetailKey(msg)
		}
		return m.handleTreeKey(msg)
	}
	return m, nil
}

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
	case "enter":
		m.detail = true
		m.detailScroll = 0
	case "tab":
		if m.detail {
			m.detail = false
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
	case "q", "esc", "tab":
		m.detail = false
	}
	return m, nil
}

// View renders the 40/60 tree/detail split.
func (m *AgentsModel) View() string {
	if m.width < 20 {
		return "agents"
	}
	treeW := m.width * 40 / 100
	detailW := m.width - treeW - 1

	treeContent := m.renderTree(treeW)
	divider := strings.Repeat("│\n", m.height)
	divider = lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Render(divider)
	detailContent := m.renderDetail(detailW)

	return lipgloss.JoinHorizontal(lipgloss.Top, treeContent, divider, detailContent)
}

func (m *AgentsModel) renderTree(width int) string {
	if len(m.nodes) == 0 {
		return lipgloss.NewStyle().Width(width).Render("No agents yet.")
	}
	var lines []string
	for i, n := range m.nodes {
		lines = append(lines, m.renderNodeRow(n, i == m.selected, width))
	}
	content := strings.Join(lines, "\n")
	return lipgloss.NewStyle().Width(width).Height(m.height).Render(content)
}

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

func (m *AgentsModel) renderNodeRow(n *agent.AgentNode, selected bool, width int) string {
	indent := strings.Repeat("  ", n.Depth)
	prefix := ""
	if n.RemoteExec {
		prefix = "☁ "
	}
	label := indent + prefix + n.Label
	glyph := glyphForStatus(n.Status)

	elapsed := ""
	if !n.StartedAt.IsZero() {
		dur := time.Since(n.StartedAt)
		if !n.EndedAt.IsZero() {
			dur = n.EndedAt.Sub(n.StartedAt)
		}
		elapsed = fmt.Sprintf("%.0fs", dur.Seconds())
	}

	tokens := fmt.Sprintf("↑%s ↓%s", formatTokens(n.TokensIn), formatTokens(n.TokensOut))
	right := glyph + " " + elapsed + "  " + tokens

	space := width - len(label) - len(right) - 4
	if space < 1 {
		space = 1
	}
	dotFill := strings.Repeat("─", space)
	row := label + "  " + dotFill + "  " + right

	if selected {
		return lipgloss.NewStyle().Reverse(true).Render(row)
	}
	return row
}

func (m *AgentsModel) renderDetail(width int) string {
	if len(m.nodes) == 0 || m.selected >= len(m.nodes) {
		return lipgloss.NewStyle().Width(width).Render("Select a node to inspect.")
	}
	n := m.nodes[m.selected]

	header := n.Label
	if n.RemoteExec {
		header += "  [remote ☁]"
		if n.RemoteJobID != "" {
			header += "  " + n.RemoteJobID
		}
	}
	glyph := glyphForStatus(n.Status)
	elapsed := ""
	if !n.StartedAt.IsZero() {
		dur := time.Since(n.StartedAt)
		if !n.EndedAt.IsZero() {
			dur = n.EndedAt.Sub(n.StartedAt)
		}
		elapsed = fmt.Sprintf("%.0fs", dur.Seconds())
	}
	cost := ""
	if n.CostUSD > 0 {
		cost = fmt.Sprintf("  $%.3f", n.CostUSD)
	}
	headerLine := fmt.Sprintf("%s  %s %s  ↑%s ↓%s%s",
		header, glyph, elapsed,
		formatTokens(n.TokensIn), formatTokens(n.TokensOut), cost)
	rule := strings.Repeat("─", width)

	var evtLines []string
	for _, evt := range n.Events {
		elapsed := ""
		if !n.StartedAt.IsZero() {
			d := evt.TS.Sub(n.StartedAt)
			elapsed = fmt.Sprintf("[%05.1fs]", d.Seconds())
		}
		line := elapsed + " ▸ " + evt.Kind.String()
		if evt.Name != "" {
			line += "  " + evt.Name
		}
		evtLines = append(evtLines, line)
	}

	allLines := append([]string{headerLine, rule}, evtLines...)
	if m.detailScroll < len(allLines) {
		allLines = allLines[m.detailScroll:]
	}
	content := strings.Join(allLines, "\n")
	return lipgloss.NewStyle().Width(width).Height(m.height).Render(content)
}
