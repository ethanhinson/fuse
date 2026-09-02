package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// This file is the single implementation of fixed-width column layout for the
// TUI's static listings (/models, the slash completer, the /models editor).
// Before it, four private renderers each re-derived the same measurement,
// padding and truncation logic; the divergence is what let change 0078's
// "global max column width not clamped to render width" defect exist in only
// one of them.
//
// Why this is not built on lipgloss/table's renderer: that renderer owns column
// sizing and reflows over-wide cells onto ADDITIONAL LINES. Three of the six
// behaviors below — the render-width clamp of a global max measured across
// off-page rows, ellipsis-reserving truncation in display cells, and the
// per-row trailing tag that must survive that truncation verbatim — are
// properties of how Fuse budgets a row, and none of them survive being handed
// to a renderer that may answer an over-wide cell with a second line. The
// measurement primitive used here is the same one lipgloss/table measures with
// (lipgloss.Width), so the cell arithmetic is shared even though the compositor
// is not. See ADR-0054 (adopt charmbracelet table/tabs as the shared TUI
// component layer), which records this boundary.

const (
	// tableDefaultGap separates adjacent columns.
	tableDefaultGap = "  "
	// tableDefaultMarker is the active-row glyph, matching the slash completer's
	// cursor so every listing reads the same way.
	tableDefaultMarker = "▸ "
	// tableLastColMin is the floor reserved for the LAST column when the total
	// natural width has to be clamped against the render width. Without it the
	// widest-first shrink can starve the trailing column (typically the
	// description) down to nothing. It never forces a column wider than its own
	// content, and it never overrides the hard clamp: a render width too small
	// for the floors is honoured, not the floors.
	tableLastColMin = 4
)

// Column describes one column of a rendered table.
type Column struct {
	// Header is the column title, emitted only when TableOpts.ShowHeader is set.
	// It participates in the column's natural width measurement.
	Header string
	// MinWidth raises the column's natural width, and is its floor when the
	// table has to shrink to fit. It is how a caller expresses a global maximum
	// measured across rows that are NOT being rendered (the slash completer's
	// registry-wide command width). It is a floor, not a guarantee: see
	// fitColumnWidths — the render width always wins.
	MinWidth int
	// MaxWidth caps the column's natural width. 0 means unbounded before the
	// render-width clamp.
	MaxWidth int
	// Blank is substituted for an empty or missing cell so the column — and
	// therefore every column after it — holds its position.
	Blank string
	// Style is applied to the PADDED cell, after measurement and truncation.
	Style lipgloss.Style
}

// Row is one rendered line of a table.
type Row struct {
	// Cells are positional against the column list. A short slice is treated as
	// trailing blanks; extra cells are ignored.
	Cells []string
	// Active renders the marker glyph; inactive rows get a width-matched indent
	// so the marker occupies budget rather than adding to it.
	Active bool
	// Tag is a trailing annotation ("(default, active)") emitted after the last
	// padded column, so tag offsets align across rows regardless of blank cells.
	Tag string
}

// TableOpts carries the table-wide knobs.
type TableOpts struct {
	// Gap separates adjacent columns. Empty means tableDefaultGap.
	Gap string
	// ActiveMarker is the plain (unstyled) glyph for an active row. Empty means
	// tableDefaultMarker. Its display width is what the inactive indent matches,
	// so it is measured unstyled and styled only at render time.
	ActiveMarker string
	// MarkerStyle styles the marker glyph. It is never measured through.
	MarkerStyle lipgloss.Style
	// ShowHeader emits a leading line built from the Column.Header values.
	ShowHeader bool
}

// RenderTable lays out rows into fixed-width columns and returns the display
// lines: an optional header followed by exactly one line per row. Every
// returned line is guaranteed to be at most width display cells.
//
// A width of 0 or less means unbounded — columns take their natural widths.
// That is only for callers with no pane to fit (an unsized viewport). Anything
// whose output reaches the screen, /models included, must pass the real width:
// the transcript word-wraps, and an over-wide row breaks inside its padding run
// and folds in two.
func RenderTable(cols []Column, rows []Row, width int, opts TableOpts) []string {
	if len(cols) == 0 {
		return nil
	}
	gap := opts.Gap
	if gap == "" {
		gap = tableDefaultGap
	}
	marker := opts.ActiveMarker
	if marker == "" {
		marker = tableDefaultMarker
	}
	markerW := lipgloss.Width(marker)
	indent := strings.Repeat(" ", markerW)

	// Pass 1 — measure, in DISPLAY CELLS, over UNSTYLED text. Cells are
	// sanitized first: untrusted model/tool bytes reaching a fixed-width pane
	// desynchronize measurement from what the terminal actually draws, and an
	// embedded newline would silently turn one row into two.
	cells := make([][]string, len(rows))
	tags := make([]string, len(rows))
	natural := make([]int, len(cols))
	for i := range cols {
		if opts.ShowHeader {
			natural[i] = lipgloss.Width(tableCell(cols[i].Header))
		}
	}
	tagW := 0
	for r, row := range rows {
		cells[r] = make([]string, len(cols))
		for c := range cols {
			v := ""
			if c < len(row.Cells) {
				v = tableCell(row.Cells[c])
			}
			if v == "" {
				v = tableCell(cols[c].Blank)
			}
			cells[r][c] = v
			if w := lipgloss.Width(v); w > natural[c] {
				natural[c] = w
			}
		}
		tags[r] = tableCell(row.Tag)
		if w := lipgloss.Width(tags[r]); w > tagW {
			tagW = w
		}
	}
	for c := range cols {
		if cols[c].MinWidth > natural[c] {
			natural[c] = cols[c].MinWidth
		}
		if cols[c].MaxWidth > 0 && natural[c] > cols[c].MaxWidth {
			natural[c] = cols[c].MaxWidth
		}
	}

	// The tag is reserved GLOBALLY (widest tag plus its leading space) rather
	// than per row, so the tag column starts at the same offset on every row and
	// no row can be pushed over the budget by its own tag.
	tagReserve := 0
	if tagW > 0 {
		tagReserve = tagW + 1
	}

	// Pass 2 — clamp the natural widths against the render width. THIS is
	// change 0078's defect: a global max measured across all rows (which is what
	// keeps gutters stable while scrolling) is not automatically a width that
	// fits on screen, and an over-wide entry that is scrolled OFF the page still
	// sets that max.
	widths := make([]int, len(cols))
	copy(widths, natural)
	if width > 0 {
		avail := width - markerW - tagReserve - lipgloss.Width(gap)*(len(cols)-1)
		fitColumnWidths(widths, columnFloors(cols, natural), avail)
	}

	out := make([]string, 0, len(rows)+1)
	if opts.ShowHeader {
		headers := make([]string, len(cols))
		for c := range cols {
			headers[c] = tableCell(cols[c].Header)
		}
		line := composeTableLine(headers, widths, cols, gap, indent, "")
		out = append(out, fitTableLine(line, width))
	}
	for r, row := range rows {
		prefix := indent
		if row.Active {
			prefix = opts.MarkerStyle.Render(marker)
		}
		line := composeTableLine(cells[r], widths, cols, gap, prefix, tags[r])
		out = append(out, fitTableLine(line, width))
	}
	return out
}

// composeTableLine builds one line: prefix, then each non-empty column padded to
// its width and separated by gap, then the tag. Truncation and padding happen on
// PLAIN text; styles are applied afterwards so no escape sequence is ever
// measured. A column clamped to zero width is omitted along with its gap, so the
// cells it would have wasted go unused rather than overflowing.
func composeTableLine(vals []string, widths []int, cols []Column, gap, prefix, tag string) string {
	var b strings.Builder
	b.WriteString(prefix)
	wrote := false
	for c := range cols {
		if widths[c] <= 0 {
			continue
		}
		if wrote {
			b.WriteString(gap)
		}
		wrote = true
		v := vals[c]
		if lipgloss.Width(v) > widths[c] {
			// Truncate the composed PLAIN string in display cells, reserving the
			// ellipsis. Any styling of this cell is applied after, so nothing is
			// cut mid-escape-sequence.
			v = truncateCells(v, widths[c])
		}
		v = padCells(v, widths[c])
		b.WriteString(cols[c].Style.Render(v))
	}
	s := b.String()
	if tag != "" {
		// Appended AFTER the last padded column, so the tag starts at the same
		// offset on every row whether or not the row has blank cells.
		return s + " " + tag
	}
	// Trailing padding is trimmed ONLY here — when no tag follows it. It is
	// invisible, and the column start offsets are unaffected by dropping it.
	return strings.TrimRight(s, " ")
}

// columnFloors returns the minimum width each column may be shrunk to before
// the hard clamp. A floor never exceeds the column's own natural width — a
// floor wider than the content would pad, not protect.
func columnFloors(cols []Column, natural []int) []int {
	floors := make([]int, len(cols))
	for c := range cols {
		f := cols[c].MinWidth
		if c == len(cols)-1 && f < tableLastColMin {
			f = tableLastColMin
		}
		if f > natural[c] {
			f = natural[c]
		}
		floors[c] = f
	}
	return floors
}

// fitColumnWidths shrinks widths in place until they sum to at most avail,
// taking a cell at a time from the widest column that is still above its floor.
// Shrinking the widest first keeps narrow columns intact and converges on an
// even split; the floors are what reserve a minimum budget for the last column.
//
// The floors are advisory, not binding: if avail cannot accommodate them the
// second pass ignores them entirely. The render width is the only hard
// constraint here, because a row that exceeds it wraps and destroys the
// alignment of every other row.
func fitColumnWidths(widths, floors []int, avail int) {
	total := 0
	for _, w := range widths {
		total += w
	}
	// Pass one honours the floors; pass two, reached only when the floors
	// themselves do not fit, runs against an all-zero floor set.
	shrink := func(floor []int) {
		for total > avail {
			best := -1
			for i, w := range widths {
				if w > floor[i] && (best < 0 || w > widths[best]) {
					best = i
				}
			}
			if best < 0 {
				return
			}
			widths[best]--
			total--
		}
	}
	shrink(floors)
	shrink(make([]int, len(widths)))
}

// fitTableLine is the final guarantee that a line fits the budget, covering the
// pathological case where width is too small even for the marker and gaps. It
// truncates ANSI-aware (escape sequences are not display cells), so a styled
// line is cut without losing its terminator.
func fitTableLine(s string, width int) string {
	if width <= 0 || lipgloss.Width(s) <= width {
		return s
	}
	return lipgloss.NewStyle().MaxWidth(width).Render(s)
}

// tableCell normalises an untrusted cell value for a fixed-width row: control
// bytes are stripped by the shared sanitizer, and any newline that survives it
// becomes a space, because one row must render as exactly one line.
func tableCell(s string) string {
	if s == "" {
		return ""
	}
	s = sanitizeDisplay(s)
	if strings.ContainsRune(s, '\n') {
		s = strings.ReplaceAll(s, "\n", " ")
	}
	return s
}

// padCells right-pads s to n DISPLAY CELLS. It never truncates — callers size n
// from the widest member of the column, or truncate to n first.
func padCells(s string, n int) string {
	if pad := n - lipgloss.Width(s); pad > 0 {
		return s + strings.Repeat(" ", pad)
	}
	return s
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
