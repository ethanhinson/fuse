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
	// completerColGap separates the command, kind-tag and description columns.
	completerColGap = "  "
	// completerCursor marks the selected row; completerIndent is its
	// width-matched filler, so the marker occupies budget rather than adding to
	// it and every column starts at the same cell on every row.
	completerCursor = "▸ "
	completerIndent = "  "
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
	// argModels, when set, supplies model-alias entries filtered by prefix. It
	// is engaged when the input is "/model <prefix>" so the argument position
	// completes aliases instead of commands.
	argModels func(prefix string) []SlashEntry
	// argMode records whether the current activation is completing an argument
	// (aliases) rather than command names, so refresh routes to the right source.
	argMode bool
}

func newSlashCompleter(reg *SlashRegistry) *slashCompleter {
	return &slashCompleter{reg: reg}
}

// withModelArg installs the alias supplier used for "/model " argument
// completion and returns the completer for chaining.
func (c *slashCompleter) withModelArg(supplier func(prefix string) []SlashEntry) *slashCompleter {
	c.argModels = supplier
	return c
}

// activate resets the completer state for a new '/' input session. When the
// input has advanced past "/model " into the argument, it switches to alias
// completion; otherwise it completes command names as before.
func (c *slashCompleter) activate(input string) {
	c.active = true
	if arg, ok := modelArgPrefix(input); ok && c.argModels != nil {
		c.argMode = true
		c.filter = arg
	} else {
		c.argMode = false
		c.filter = filterFrom(input)
	}
	c.refresh()
}

// modelArgPrefix reports whether input is completing the argument of the
// /model command, returning the (possibly empty) alias prefix typed so far.
// It matches only when the command token is exactly "/model" followed by a
// space, so "/models" and "/model" (no space yet) fall through to command
// completion.
func modelArgPrefix(input string) (string, bool) {
	const cmd = "/model "
	if !strings.HasPrefix(input, cmd) {
		return "", false
	}
	arg := input[len(cmd):]
	// A second space means the user has moved past the alias token; stop
	// offering completions rather than matching against a multi-word filter.
	if strings.Contains(arg, " ") {
		return "", false
	}
	return arg, true
}

// deactivate hides the overlay.
func (c *slashCompleter) deactivate() {
	c.active = false
	c.argMode = false
	c.cursor = 0
	c.offset = 0
	c.filter = ""
	c.visible = nil
}

// refresh re-queries the active source (aliases in argument mode, else the
// command registry) and resets cursor/offset when results change.
func (c *slashCompleter) refresh() {
	if c.argMode && c.argModels != nil {
		c.visible = c.argModels(c.filter)
	} else {
		c.visible = c.reg.Filter(c.filter)
	}
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
	page := c.visible[c.offset:end]

	cmdW := completerCommandWidth(c.reg, width)

	// Layout is the shared table primitive's (table.go). The two invariants this
	// overlay contributes are expressed declaratively as column bounds:
	//
	//   - the command column is pinned to cmdW by BOTH MinWidth and MaxWidth.
	//     MinWidth is what makes the width registry-wide rather than page-wide:
	//     the table measures only the rows it is given, and the widest command
	//     is routinely scrolled off the page.
	//   - the description column's MinWidth is the budget the clamp reserves for
	//     it, so the table's shrink-the-widest pass stops there instead of
	//     starving it.
	//
	// The kind tag is a fixed-width column rather than a fmt pad. Everything
	// after this point is styling, not measurement.
	cols := []Column{
		{MinWidth: cmdW, MaxWidth: cmdW},
		{MinWidth: kindTagWidth, MaxWidth: kindTagWidth, Style: completerKindStyle},
		{MinWidth: completerMinDescCells},
	}
	rows := make([]Row, len(page))
	for i, e := range page {
		// commandText remains the single source of truth for the command
		// portion — the same function MaxCommandWidth measures through — so the
		// per-row cell can never drift from the registry max. Never a styled
		// string: escape sequences are not display cells.
		cmd := commandText(e)
		if cmdW == 0 {
			// The clamp left no room at all (a terminal narrower than the fixed
			// furniture). Drop the command rather than let the column take its
			// natural width, which MaxWidth: 0 would mean.
			cmd = ""
		}
		rows[i] = Row{
			Cells:  []string{cmd, e.KindTag(), e.Description},
			Active: c.offset+i == c.cursor,
		}
	}
	lines := RenderTable(cols, rows, width, TableOpts{
		Gap:          completerColGap,
		ActiveMarker: completerCursor,
	})

	var b strings.Builder
	for i, line := range lines {
		b.WriteString(completerStyleCommand(line, page[i], c.offset+i == c.cursor, cmdW))
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

// completerCommandWidth is the width of the command column: the widest command
// portion across the WHOLE registry — not the visible page — so the gutter does
// not shift as the window scrolls, clamped against the render width.
//
// The clamp is change 0078's shipped defect and the reason this is a named
// function rather than an inline expression. The registry includes MCP tool
// names ("/mcp:server/tool") routinely wider than the whole row, and one such
// entry sets the registry max even while scrolled off the page. Unclamped,
// EVERY row becomes 2+max+2+kindTagWidth+2 cells regardless of its own command,
// and the shell composites this overlay through wordwrap — which breaks at the
// last space before the limit, i.e. INSIDE the pad run, spilling the kind tag
// and description onto a second line for every entry.
//
// The subtraction is the row's fixed furniture: the 2-cell cursor, the two
// 2-cell gaps, the kind-tag column, and the description budget.
func completerCommandWidth(reg *SlashRegistry, width int) int {
	maxCmd := 0
	if reg != nil {
		maxCmd = reg.MaxCommandWidth()
	}
	capCmd := width - (2 + 2 + kindTagWidth + 2 + completerMinDescCells)
	if maxCmd > capCmd {
		maxCmd = capCmd
	}
	if maxCmd < 0 {
		maxCmd = 0
	}
	return maxCmd
}

// completerStyleCommand re-applies the completer's styling to the command
// portion of a line already laid out by RenderTable. It is a pure substitution
// of the leading marker+command run: same display width in, same display width
// out, so it cannot disturb the table's measurement.
//
// It exists because both styles cover a SUBSTRING of the command cell, which a
// Column.Style (applied to the whole padded cell) cannot express: the selected
// highlight covers marker+command but NOT the trailing pad, and the syntax
// highlight covers only the Syntax token. Nesting them as row/column styles
// would also let an inner reset terminate the outer style mid-line.
//
// A truncated command portion keeps the documented styling loss: it was cut by
// the table as one composed PLAIN string, so there is no longer a Syntax token
// to highlight.
func completerStyleCommand(line string, e SlashEntry, selected bool, cmdW int) string {
	if cmdW <= 0 {
		return line
	}
	marker := completerIndent
	if selected {
		marker = completerCursor
	}
	cell := commandText(e)
	truncated := commandWidth(e) > cmdW
	if truncated {
		cell = truncateCells(cell, cmdW)
	}
	// The plain run the table emitted for this cell. If it does not match — the
	// sanitizer rewrote the text, or fitTableLine cut the row short — leave the
	// line alone rather than splice at the wrong offset.
	plain := marker + padCells(cell, cmdW)
	if !strings.HasPrefix(line, plain) {
		return line
	}
	// truncateCells may land a cell short of the budget next to a double-width
	// rune, so pad to the target rather than assume the text fills it.
	pad := strings.Repeat(" ", cmdW-lipgloss.Width(cell))

	head := marker + cell
	tail := ""
	if !truncated && e.Syntax != "" {
		head = marker + e.Command
		tail = " " + completerSyntaxStyle.Render(e.Syntax)
	}
	if selected {
		head = completerSelectedStyle.Render(head)
	}
	return head + tail + pad + line[len(plain):]
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
