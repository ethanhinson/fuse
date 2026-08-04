package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

const (
	completerMaxRows  = 8
	kindTagWidth      = 18
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

// update re-filters based on the latest input value.
func (c *slashCompleter) update(input string) {
	if !strings.HasPrefix(input, "/") {
		c.deactivate()
		return
	}
	c.active = true
	newFilter := filterFrom(input)
	if newFilter != c.filter {
		c.filter = newFilter
		c.refresh()
	}
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

	var b strings.Builder
	for i := c.offset; i < end; i++ {
		e := c.visible[i]
		cursor := "  "
		if i == c.cursor {
			cursor = "▸ "
		}

		// Build the label: command + optional syntax
		label := e.Command
		if e.Syntax != "" {
			label += " " + completerSyntaxStyle.Render(e.Syntax)
		}

		// Kind tag right-aligned in fixed column
		tag := e.KindTag()
		paddedTag := fmt.Sprintf("%-*s", kindTagWidth, tag)

		// Description truncated to remaining width
		used := len(cursor) + len(e.Command) + 2 + kindTagWidth + 2
		descWidth := width - used
		desc := e.Description
		if descWidth > 0 && len(desc) > descWidth {
			desc = desc[:descWidth-1] + "…"
		}

		line := cursor + label + "  " + completerKindStyle.Render(paddedTag) + "  " + desc
		if i == c.cursor {
			line = completerSelectedStyle.Render(cursor+e.Command) +
				(func() string {
					if e.Syntax != "" {
						return " " + completerSyntaxStyle.Render(e.Syntax)
					}
					return ""
				})() +
				"  " + completerKindStyle.Render(paddedTag) + "  " + desc
		}
		b.WriteString(line)
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
