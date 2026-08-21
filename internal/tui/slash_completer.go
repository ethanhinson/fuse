package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

const (
	completerMaxRows = 8
	kindTagWidth     = 18
	// completerMinDescCells is the description budget reserved when clamping
	// the registry-wide command column against the terminal width.
	completerMinDescCells = 8
)

var (
	completerSelectedStyle = lipgloss.NewStyle().Background(lipgloss.Color("#2c313a")).Foreground(colCyan)
	completerNormalStyle   = lipgloss.NewStyle().Foreground(colNormal)
	completerKindStyle     = lipgloss.NewStyle().Foreground(colMuted)
	completerSyntaxStyle   = lipgloss.NewStyle().Foreground(colAmber)
	completerScrollStyle   = lipgloss.NewStyle().Foreground(colMuted)
)

// slashCompleter is a filterable overlay driven by ShellModel.Update.
// It is not a tea.Model itself — the shell model owns it as a plain struct.
type slashCompleter struct {
	reg     *SlashRegistry
	filter  string
	visible []SlashEntry
	cursor  int
	active  bool
	offset  int // scroll offset for >completerMaxRows items
}

func newSlashCompleter(reg *SlashRegistry) *slashCompleter {
	return &slashCompleter{reg: reg}
}

// activate resets the completer state for a new '/' input session.
func (c *slashCompleter) activate(input string) {
	c.active = true
	c.filter = filterFrom(input)
	c.refresh()
}

// deactivate hides the overlay.
func (c *slashCompleter) deactivate() {
	c.active = false
	c.cursor = 0
	c.offset = 0
	c.filter = ""
	c.visible = nil
}

// refresh re-queries the registry and resets cursor/offset when results change.
func (c *slashCompleter) refresh() {
	c.visible = c.reg.Filter(c.filter)
	if c.cursor >= len(c.visible) {
		c.cursor = 0
	}
	c.clampOffset()
}

// filterFrom extracts the filter text from the input (everything after '/').
func filterFrom(input string) string {
	if len(input) > 1 {
		return input[1:]
	}
	return ""
}

// moveUp moves the cursor up one row (wrapping).
func (c *slashCompleter) moveUp() {
	if len(c.visible) == 0 {
		return
	}
	c.cursor--
	if c.cursor < 0 {
		c.cursor = len(c.visible) - 1
	}
	c.clampOffset()
}

// moveDown moves the cursor down one row (wrapping).
func (c *slashCompleter) moveDown() {
	if len(c.visible) == 0 {
		return
	}
	c.cursor = (c.cursor + 1) % len(c.visible)
	c.clampOffset()
}

func (c *slashCompleter) clampOffset() {
	if c.cursor < c.offset {
		c.offset = c.cursor
	} else if c.cursor >= c.offset+completerMaxRows {
		c.offset = c.cursor - completerMaxRows + 1
	}
}

// selected returns the entry under the cursor, or zero value if none.
func (c *slashCompleter) selected() SlashEntry {
	if len(c.visible) == 0 {
		return SlashEntry{}
	}
	return c.visible[c.cursor]
}

// View renders the overlay as a string to be appended above the input line.
// Returns "" when inactive or no entries match.
func (c *slashCompleter) View(width int) string {
	if !c.active || len(c.visible) == 0 {
		return ""
	}

	end := c.offset + completerMaxRows
	if end > len(c.visible) {
		end = len(c.visible)
	}

	// Width of the widest command portion across the WHOLE registry, so the
	// gutter does not shift as the visible window scrolls. Measured in display
	// cells against unstyled text.
	maxCmd := 0
	if c.reg != nil {
		maxCmd = c.reg.MaxCommandWidth()
	}
	// Clamp the registry-wide max against the terminal width ONCE, before the
	// loop. The registry includes MCP tool names ("/mcp:server/tool") that are
	// routinely wider than the whole row; without this every row would be
	// 2+maxCmd+2+kindTagWidth+2 cells regardless of its own command, and the
	// shell composites this overlay through wordwrap — which breaks at the last
	// space before the limit, i.e. inside the pad run, spilling the kind tag and
	// description onto a second line for every entry.
	if capCmd := width - (2 + 2 + kindTagWidth + 2 + completerMinDescCells); maxCmd > capCmd {
		maxCmd = capCmd
		if maxCmd < 0 {
			maxCmd = 0
		}
	}

	var b strings.Builder
	for i := c.offset; i < end; i++ {
		e := c.visible[i]
		cursor := "  "
		if i == c.cursor {
			cursor = "▸ "
		}

		// Measure the plain command portion through commandWidth, the same
		// function MaxCommandWidth uses, so the per-row pad can never drift
		// from the registry max. Never measure a styled string: the escape
		// sequences are not display cells.
		var label string
		cmdCells := commandWidth(e)
		if cmdCells > maxCmd {
			// Over the clamped budget: truncate the composed plain text. This
			// loses the syntax highlight, but a row that overflows the width
			// wraps and destroys the alignment for every other row.
			plain := truncateCells(commandText(e), maxCmd)
			cmdCells = lipgloss.Width(plain)
			label = cursor + plain
			if i == c.cursor {
				label = completerSelectedStyle.Render(label)
			}
		} else {
			head := cursor + e.Command
			if i == c.cursor {
				head = completerSelectedStyle.Render(head)
			}
			label = head
			if e.Syntax != "" {
				label += " " + completerSyntaxStyle.Render(e.Syntax)
			}
		}

		// One pad computation for both the selected and normal rows, so it can
		// never be applied to only one of them. truncateCells may land a cell
		// short of the budget next to a double-width rune, so pad to the target
		// rather than assuming the truncated text fills it.
		pad := maxCmd - cmdCells
		if pad < 0 {
			pad = 0
		}
		label += strings.Repeat(" ", pad)

		// Kind tag left-aligned in a fixed column
		tag := e.KindTag()
		paddedTag := fmt.Sprintf("%-*s", kindTagWidth, tag)

		// Description truncated to the remaining cells.
		used := lipgloss.Width(cursor) + maxCmd + 2 + kindTagWidth + 2
		descWidth := width - used
		desc := ""
		if descWidth > 0 {
			desc = truncateCells(e.Description, descWidth)
		}

		b.WriteString(label + "  " + completerKindStyle.Render(paddedTag) + "  " + desc)
		b.WriteByte('\n')
	}

	// Scroll indicators
	if c.offset > 0 {
		above := c.offset
		b.WriteString(completerScrollStyle.Render(fmt.Sprintf("  ↑ %d more", above)))
		b.WriteByte('\n')
	}
	if end < len(c.visible) {
		below := len(c.visible) - end
		b.WriteString(completerScrollStyle.Render(fmt.Sprintf("  ↓ %d more", below)))
		b.WriteByte('\n')
	}

	return b.String()
}

// commandText composes an entry's command portion as plain, unstyled text.
// Single source of truth for the string itself: both commandWidth (and through
// it the registry max) and View's per-row pad go through this, so a change to
// the separator cannot desynchronize the two.
func commandText(e SlashEntry) string {
	if e.Syntax != "" {
		return e.Command + " " + e.Syntax
	}
	return e.Command
}

// commandWidth is the display-cell width of an entry's command portion,
// measured UNSTYLED. Single source of truth for both the registry max and
// the per-row pad.
func commandWidth(e SlashEntry) int {
	return lipgloss.Width(commandText(e))
}
