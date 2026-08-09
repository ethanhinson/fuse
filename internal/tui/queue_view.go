package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ethanhinson/fuse/internal/agent"
)

// queue_view.go: the pending-message queue editor (ADR-0022 feature 4). Opened
// with /queue, it lists every human message still waiting for delivery, grouped
// by target node, and lets the human delete, reorder, or edit an item before it
// drains at a turn boundary. It is a lightweight in-transcript editor (not a
// full-screen overlay): j/k select, d delete, J/K move down/up, e edit, Esc close.

// queueState is the transient editor state. It is nil unless /queue is open.
type queueState struct {
	rows   []queueRow
	cursor int
	// editing holds the in-progress edit buffer when non-nil.
	editing *string
}

// queueRow is one selectable pending message, flattened across nodes for display
// (node grouping is shown via the Handle column).
type queueRow struct {
	nodeID string
	msgID  string
	handle string
	text   string
	mode   agent.MsgMode
}

// snapshotQueue flattens the bus's per-node pending queues into display rows,
// ordered by node then Seq. Handles are looked up fresh so renames show.
func snapshotQueue(bus *agent.HumanBus, reg *agent.HandleRegistry) []queueRow {
	if bus == nil {
		return nil
	}
	all := bus.PendingAll()
	// Stable order: sort node IDs, then each node's Seq-ordered slice.
	nodeIDs := make([]string, 0, len(all))
	for id := range all {
		nodeIDs = append(nodeIDs, id)
	}
	sortStrings(nodeIDs)
	var rows []queueRow
	for _, id := range nodeIDs {
		handle := id
		if reg != nil {
			if h, ok := reg.HandleFor(id); ok {
				handle = h
			}
		}
		for _, msg := range all[id] {
			rows = append(rows, queueRow{nodeID: id, msgID: msg.ID, handle: handle, text: msg.Text, mode: msg.Mode})
		}
	}
	return rows
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// openQueue builds the editor state from the current bus contents.
func (m *ShellModel) openQueue() {
	m.queue = &queueState{rows: snapshotQueue(m.humanBus, m.handleReg)}
}

// refreshQueue re-reads the bus after a mutation, clamping the cursor.
func (m *ShellModel) refreshQueue() {
	if m.queue == nil {
		return
	}
	m.queue.rows = snapshotQueue(m.humanBus, m.handleReg)
	if m.queue.cursor >= len(m.queue.rows) {
		m.queue.cursor = len(m.queue.rows) - 1
	}
	if m.queue.cursor < 0 {
		m.queue.cursor = 0
	}
}

// handleQueueKey drives the queue editor. Returns handled=false for keys the
// editor does not own so they fall through.
func (m ShellModel) handleQueueKey(msg tea.KeyMsg) (handled bool, model tea.Model, cmd tea.Cmd) {
	if m.queue == nil {
		return false, m, nil
	}
	q := m.queue

	// Edit mode captures typing until Enter/Esc.
	if q.editing != nil {
		switch msg.Type {
		case tea.KeyEnter:
			if len(q.rows) > 0 {
				row := q.rows[q.cursor]
				m.humanBus.Edit(row.nodeID, row.msgID, strings.TrimSpace(*q.editing))
			}
			q.editing = nil
			m.refreshQueue()
			return true, m, nil
		case tea.KeyEsc:
			q.editing = nil
			return true, m, nil
		case tea.KeyBackspace:
			if n := len(*q.editing); n > 0 {
				*q.editing = (*q.editing)[:n-1]
			}
			return true, m, nil
		case tea.KeyRunes, tea.KeySpace:
			if msg.Type == tea.KeySpace {
				*q.editing += " "
			} else {
				*q.editing += string(msg.Runes)
			}
			return true, m, nil
		default:
			return true, m, nil
		}
	}

	switch msg.String() {
	case "esc", "q":
		m.queue = nil
		return true, m, nil
	case "up", "k":
		if q.cursor > 0 {
			q.cursor--
		}
		return true, m, nil
	case "down", "j":
		if q.cursor < len(q.rows)-1 {
			q.cursor++
		}
		return true, m, nil
	case "d", "x":
		if len(q.rows) > 0 {
			row := q.rows[q.cursor]
			m.humanBus.Delete(row.nodeID, row.msgID)
			m.appendLine(headerStyle.Render("deleted queued message"))
			m.refreshQueue()
		}
		return true, m, nil
	case "K", "shift+up":
		m.moveQueued(-1)
		return true, m, nil
	case "J", "shift+down":
		m.moveQueued(1)
		return true, m, nil
	case "e":
		if len(q.rows) > 0 {
			buf := q.rows[q.cursor].text
			q.editing = &buf
		}
		return true, m, nil
	}
	return true, m, nil // editor swallows other keys while open
}

// moveQueued reorders the selected message within its node's queue by delta.
func (m *ShellModel) moveQueued(delta int) {
	q := m.queue
	if q == nil || len(q.rows) == 0 {
		return
	}
	row := q.rows[q.cursor]
	// Compute the message's index within its own node's rows.
	idx := 0
	for _, r := range q.rows {
		if r.nodeID == row.nodeID {
			if r.msgID == row.msgID {
				break
			}
			idx++
		}
	}
	m.humanBus.Move(row.nodeID, row.msgID, idx+delta)
	m.refreshQueue()
	// Keep the cursor on the moved row.
	for i, r := range q.rows {
		if r.msgID == row.msgID {
			q.cursor = i
			break
		}
	}
}

// renderQueueOverlay paints the queue editor over the bottom of the viewport.
func renderQueueOverlay(base string, q *queueState, width int) string {
	if q == nil || width < 8 {
		return base
	}
	var lines []string
	add := func(s string) { lines = append(lines, s) }
	add(" " + askHeaderStyle.Render("⋯ Message queue") + headerStyle.Render(fmt.Sprintf("  (%d pending)", len(q.rows))))
	add("")
	if len(q.rows) == 0 {
		add(" " + headerStyle.Render("no pending messages"))
	}
	for i, row := range q.rows {
		cursor := "  "
		if i == q.cursor {
			cursor = askCursorStyle.Render("❯ ")
		}
		tag := humanMsgStyle.Render(row.handle)
		if row.mode == agent.ModeBroadcast {
			tag = humanMsgStyle.Render("@all")
		}
		add(fmt.Sprintf("%s%s  %s", cursor, tag, sanitizeDisplay(truncate(row.text, width-24))))
	}
	add("")
	if q.editing != nil {
		add(" " + askKeysStyle.Render("edit:"))
		add("   " + askSelectedStyle.Render(*q.editing+"▌"))
		add(" " + askKeysStyle.Render("Enter save · Esc cancel"))
	} else {
		add(" " + askKeysStyle.Render("j/k move · e edit · d delete · J/K reorder · Esc close"))
	}

	overlay := lines
	baseLines := strings.Split(base, "\n")
	start := len(baseLines) - len(overlay)
	if start < 0 {
		start = 0
	}
	for i, ol := range overlay {
		if start+i < len(baseLines) {
			baseLines[start+i] = fitLine(ol, width)
		}
	}
	return strings.Join(baseLines, "\n")
}
