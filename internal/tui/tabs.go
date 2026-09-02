package tui

import (
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// This file is the tabbed-container primitive behind /config (change 0080). It
// owns only two things: which pane is active, and a tab bar that fits the
// caller's width. Everything else — what a pane draws, what keys it answers,
// and when the container closes — belongs to the host.
//
// Width invariant (change 0041's finding, and the reason the bar is composed by
// hand): every separator glyph in the bar spends a cell from the SAME budget the
// titles spend. A glyph REPLACES a content column, it never adds one. So the bar
// is never wrapped in lipgloss.Border and never joined with
// lipgloss.JoinHorizontal — that helper cannot constrain per-line width when
// styled content overruns its allocation, which is exactly why
// agents_model.go's two-pane join is also written by hand. Title widths are
// fitted through fitColumnWidths (table.go) against the budget that remains
// AFTER the separators are paid for, and the composed bar passes through
// fitTableLine as a final hard guarantee.

const (
	// tabsSeparator sits between adjacent tab labels. Its cells come out of the
	// bar's budget like any other content.
	tabsSeparator = " │ "
	// tabsMinLabel is the floor a tab label is shrunk to before it is dropped
	// from the bar entirely; below this a label is all ellipsis and says nothing.
	tabsMinLabel = 3
)

// Pane is one tab of a Tabs container.
type Pane struct {
	// Title labels the tab in the bar. It is truncated to fit; it is never
	// allowed to widen the bar past the render width.
	Title string
	// View renders the pane body into the content area Tabs allocates it, which
	// is the container's width and its height minus the bar and rule rows. It
	// may return multiple lines; Tabs fits each one and clips to the height.
	View func(width, height int) string
	// Key answers a key the container did not claim for itself. A nil Key means
	// the pane is inert: the key is reported unhandled and travels back to the
	// host. Returning false from a non-nil Key has the same effect.
	Key func(tea.KeyMsg) (handled bool, cmd tea.Cmd)
}

// Tabs is a container that renders a tab bar over one active pane.
//
// Key bindings (settled against shell_model.go's overlay guard order, where
// approvals and asks are dispatched BEFORE the active overlay ever sees a key —
// this component changes nothing about that priority):
//
//   - tab / shift+tab — next / previous tab, wrapping in both directions.
//   - 1..9 — jump to that tab, 1-indexed. An index with no pane is not claimed.
//   - esc — deliberately NOT handled, and deliberately NOT delegated: it is the
//     host's close key (matching /queue and the models editor), and delegating
//     it would let a pane swallow the close.
//   - anything else — delegated to the active pane.
//
// The zero value is a usable empty container.
type Tabs struct {
	panes  []Pane
	active int
}

// NewTabs builds a container over the given panes, with the first one active.
func NewTabs(panes ...Pane) *Tabs {
	return &Tabs{panes: panes}
}

// Len reports how many panes the container holds.
func (t *Tabs) Len() int { return len(t.panes) }

// Active reports the index of the active pane, or 0 when there are none.
func (t *Tabs) Active() int { return t.active }

// ActivePane returns the active pane and whether one exists.
func (t *Tabs) ActivePane() (Pane, bool) {
	if t.active < 0 || t.active >= len(t.panes) {
		return Pane{}, false
	}
	return t.panes[t.active], true
}

// SetActive selects a pane by index. An out-of-range index is a no-op, so a
// caller may route a raw digit here without bounds-checking it first.
func (t *Tabs) SetActive(i int) {
	if i < 0 || i >= len(t.panes) {
		return
	}
	t.active = i
}

// Next advances to the following tab, wrapping past the last.
func (t *Tabs) Next() {
	if len(t.panes) == 0 {
		return
	}
	t.active = (t.active + 1) % len(t.panes)
}

// Prev retreats to the preceding tab, wrapping past the first.
func (t *Tabs) Prev() {
	if len(t.panes) == 0 {
		return
	}
	t.active = (t.active - 1 + len(t.panes)) % len(t.panes)
}

// Update handles a key, reporting whether the container or its active pane
// consumed it. An unconsumed key — notably esc — is the host's to act on.
func (t *Tabs) Update(msg tea.KeyMsg) (bool, tea.Cmd) {
	if len(t.panes) == 0 {
		return false, nil
	}
	switch msg.String() {
	case "tab":
		t.Next()
		return true, nil
	case "shift+tab":
		t.Prev()
		return true, nil
	case "esc":
		// Not ours, and not the pane's: the host closes on esc.
		return false, nil
	}
	// Direct index keys. A digit naming a pane that does not exist is left to
	// the active pane rather than swallowed, so panes keep the digits the
	// container has no use for.
	if msg.Type == tea.KeyRunes && len(msg.Runes) == 1 {
		if r := msg.Runes[0]; r >= '1' && r <= '9' {
			if i := int(r - '1'); i < len(t.panes) {
				t.active = i
				return true, nil
			}
		}
	}
	pane := t.panes[t.active]
	if pane.Key == nil {
		return false, nil
	}
	return pane.Key(msg)
}

// View renders the tab bar, a rule, and the active pane's body. Every returned
// line is at most width display cells, and the whole render is at most height
// lines.
func (t *Tabs) View(width, height int) string {
	if len(t.panes) == 0 || width <= 0 || height <= 0 {
		return ""
	}
	lines := []string{t.barLine(width)}
	if height > 1 {
		lines = append(lines, lipgloss.NewStyle().Foreground(colMuted).
			Render(strings.Repeat("─", width)))
	}
	bodyH := height - len(lines)
	if bodyH > 0 {
		pane := t.panes[t.active]
		body := ""
		if pane.View != nil {
			body = pane.View(width, bodyH)
		}
		if body != "" {
			head := len(lines)
			for _, line := range strings.Split(body, "\n") {
				if len(lines)-head >= bodyH {
					break
				}
				lines = append(lines, fitTableLine(line, width))
			}
		}
	}
	return strings.Join(lines, "\n")
}

// barLine composes the tab bar by hand and fits it to width. Separators are
// paid for out of the same budget as the labels (change 0041), and labels are
// shrunk widest-first through table.go's fitColumnWidths so a single long title
// cannot starve its neighbours.
func (t *Tabs) barLine(width int) string {
	labels := make([]string, len(t.panes))
	widths := make([]int, len(t.panes))
	for i, p := range t.panes {
		labels[i] = strconv.Itoa(i+1) + " " + tableCell(p.Title)
		widths[i] = lipgloss.Width(labels[i])
	}
	// The separators are spent BEFORE the labels get a budget: a glyph replaces
	// a content column rather than adding one.
	avail := width - lipgloss.Width(tabsSeparator)*(len(labels)-1)
	if avail < 0 {
		avail = 0
	}
	fitColumnWidths(widths, make([]int, len(widths)), avail)

	sep := lipgloss.NewStyle().Foreground(colMuted).Render(tabsSeparator)
	activeStyle := lipgloss.NewStyle().Foreground(colCyan).Bold(true)
	idleStyle := lipgloss.NewStyle().Foreground(colMuted)

	var b strings.Builder
	wrote := false
	for i, label := range labels {
		if widths[i] < tabsMinLabel {
			continue
		}
		if wrote {
			b.WriteString(sep)
		}
		wrote = true
		// Truncate the PLAIN label, then style: escape sequences are not cells.
		text := truncateCells(label, widths[i])
		if i == t.active {
			b.WriteString(activeStyle.Render(text))
		} else {
			b.WriteString(idleStyle.Render(text))
		}
	}
	return fitTableLine(b.String(), width)
}
