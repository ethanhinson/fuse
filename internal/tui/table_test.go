package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// cellOffsetOf returns the display-cell offset at which sub starts in line, or
// -1 when sub is absent. Offsets are measured in CELLS (lipgloss.Width of the
// prefix), never bytes or runes — that is what "column alignment" means here.
func cellOffsetOf(t *testing.T, line, sub string) int {
	t.Helper()
	i := strings.Index(line, sub)
	if i < 0 {
		return -1
	}
	return lipgloss.Width(line[:i])
}

// fieldOffsets returns the cell offset of the start of each whitespace-delimited
// field on a plain line, skipping the active marker glyph. This mirrors the scan
// in models_command_test.go's TestModelsListingColumnOffsets so the primitive is
// held to the assertion the adopted renderer already passes.
func fieldOffsets(line string) []int {
	var out []int
	runes := []rune(line)
	inField := false
	for j := 0; j < len(runes); j++ {
		isSpace := runes[j] == ' '
		if !isSpace && runes[j] != '▸' && !inField {
			inField = true
			out = append(out, lipgloss.Width(string(runes[:j])))
		} else if isSpace {
			inField = false
		}
	}
	return out
}

// TestTableRowsFitWidth is the change-0078 regression: a global-max column width
// derived from an entry that is WIDER THAN THE BUDGET and SCROLLED OFF the
// visible page must still be clamped against the render width. An
// alignment-only assertion is blind to this, so this asserts the width of every
// emitted line directly.
func TestTableRowsFitWidth(t *testing.T) {
	// The off-page over-wide entry enters the primitive exactly the way the
	// slash completer feeds it: as a registry-wide MinWidth measured across ALL
	// entries, not just the rows being rendered.
	const offPageWidth = 200
	cols := []Column{
		{Header: "command", MinWidth: offPageWidth},
		{Header: "kind"},
		{Header: "description", MinWidth: 8},
	}
	rows := []Row{
		{Cells: []string{"/model", "builtin", "switch the active model"}, Active: true, Tag: "(default, active)"},
		{Cells: []string{"/models", "builtin", "list configured models"}},
		// An over-wide entry that IS on the page, as well.
		{Cells: []string{
			"/mcp:some-very-long-server-name/some-very-long-tool-name-that-overflows-everything",
			"mcp",
			"a description that is itself far longer than any terminal is ever going to be, on purpose",
		}},
		// Untrusted bytes: a newline would shear the row count and an ESC would
		// desynchronise measurement from what the terminal draws.
		{Cells: []string{"/weird\x1b[31m", "builtin\nfake-row", "tab\there"}},
		{Cells: []string{"日本語のコマンド名がとても長い場合", "mcp", "説明文もまた非常に長い場合がありますよ"}, Tag: "(active)"},
	}

	for _, width := range []int{40, 60, 80, 100, 120} {
		lines := RenderTable(cols, rows, width, TableOpts{ShowHeader: true})
		if len(lines) != len(rows)+1 {
			t.Fatalf("width %d: got %d lines, want %d (header + one per row)", width, len(lines), len(rows)+1)
		}
		for i, line := range lines {
			if w := lipgloss.Width(line); w > width {
				t.Errorf("width %d: line %d is %d cells (over budget): %q", width, i, w, line)
			}
			if strings.ContainsAny(line, "\n\x1b\t") {
				t.Errorf("width %d: line %d carries a control byte: %q", width, i, line)
			}
		}
		// The width assertion above is necessary but NOT sufficient: the final
		// ANSI-aware clamp in fitTableLine would satisfy it even with the column
		// clamp defeated (verified by mutation). The witness that the columns —
		// not the safety net — did the fitting is that each row's trailing tag
		// survives verbatim, which a right-hand truncation destroys.
		for i, row := range rows {
			if row.Tag == "" {
				continue
			}
			if !strings.Contains(lines[i+1], row.Tag) {
				t.Errorf("width %d: tag %q lost from row %q", width, row.Tag, lines[i+1])
			}
		}
	}
}

// TestTableTagSurvivesTruncation is the change-0066 regression: a width
// invariant is the WRONG witness for truncation (fitLine produces the very
// width the assertion checks), so the witness is that the trailing tag survives
// VERBATIM. The pure-ASCII case is the one that catches a byte-vs-cell budget
// bug; the CJK/emoji case alone passes for the wrong reason.
func TestTableTagSurvivesTruncation(t *testing.T) {
	cases := []struct {
		name string
		wide string
		tag  string
	}{
		{
			name: "pure ascii",
			wide: strings.Repeat("abcdefghij", 12), // 120 plain ASCII cells
			tag:  "(default, active)",
		},
		{
			name: "cjk and emoji",
			wide: strings.Repeat("日本語🎉", 30),
			tag:  "(active)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cols := []Column{{Header: "wide"}, {Header: "second"}}
			rows := []Row{
				{Cells: []string{tc.wide, "second"}, Tag: tc.tag},
				{Cells: []string{"short", "second"}},
			}
			for _, width := range []int{40, 60, 80} {
				lines := RenderTable(cols, rows, width, TableOpts{})
				if len(lines) != 2 {
					t.Fatalf("width %d: got %d lines, want 2", width, len(lines))
				}
				if !strings.Contains(lines[0], tc.tag) {
					t.Errorf("width %d: tag %q lost from row %q", width, tc.tag, lines[0])
				}
				for i, line := range lines {
					if w := lipgloss.Width(line); w > width {
						t.Errorf("width %d: line %d is %d cells: %q", width, i, w, line)
					}
				}
			}
		})
	}
}

// TestTableBlankCellHoldsColumn asserts a missing middle value does not collapse
// the gutter: the FOLLOWING column starts at the same cell offset whether the
// middle cell is present or blank.
func TestTableBlankCellHoldsColumn(t *testing.T) {
	for _, blank := range []string{"", "-"} {
		t.Run("blank="+blank, func(t *testing.T) {
			cols := []Column{
				{Header: "alias"},
				{Header: "persona", Blank: blank},
				{Header: "trailing"},
			}
			rows := []Row{
				{Cells: []string{"glm", "reviewer", "SENTINEL"}},
				{Cells: []string{"sonnet", "", "SENTINEL"}},
				// A short row that omits the cell entirely, not just empties it.
				{Cells: []string{"haiku"}},
			}
			lines := RenderTable(cols, rows, 0, TableOpts{})
			if len(lines) != 3 {
				t.Fatalf("got %d lines, want 3", len(lines))
			}
			withVal := cellOffsetOf(t, lines[0], "SENTINEL")
			without := cellOffsetOf(t, lines[1], "SENTINEL")
			if withVal < 0 || without < 0 {
				t.Fatalf("sentinel missing: %q / %q", lines[0], lines[1])
			}
			if withVal != without {
				t.Fatalf("trailing column starts at cell %d with a value but %d when blank\nrow0=%q\nrow1=%q",
					withVal, without, lines[0], lines[1])
			}
		})
	}
}

// TestTableColumnOffsetsStable mirrors models_command_test.go's column-offset
// assertions: every row's field start offsets are identical, including the row
// carrying the active marker.
func TestTableColumnOffsetsStable(t *testing.T) {
	cols := []Column{{Header: "alias"}, {Header: "id"}, {Header: "persona"}}
	rows := []Row{
		{Cells: []string{"glm", "zai/glm-4.6", "reviewer"}, Active: true, Tag: "(default, active)"},
		{Cells: []string{"sonnet-5", "anthropic/claude-sonnet-5", "planner"}},
		{Cells: []string{"haiku", "anthropic/claude-haiku", "scout"}, Tag: "(default)"},
	}
	lines := RenderTable(cols, rows, 0, TableOpts{})
	var first []int
	for i, line := range lines {
		got := fieldOffsets(line)
		if len(got) < 3 {
			t.Fatalf("row %d %q: found %d fields, want >= 3", i, line, len(got))
		}
		got = got[:3]
		if i == 0 {
			first = got
			continue
		}
		for c := range got {
			if got[c] != first[c] {
				t.Fatalf("row %d offsets %v != row 0 offsets %v\nrow0=%q\nrowN=%q", i, got, first, lines[0], line)
			}
		}
	}
}

// TestTableActiveMarkerWidthNeutral asserts the marker occupies budget rather
// than adding to it: an active row and an inactive row with identical cells are
// the same number of display cells wide.
func TestTableActiveMarkerWidthNeutral(t *testing.T) {
	cols := []Column{{Header: "alias"}, {Header: "id"}}
	rows := []Row{
		{Cells: []string{"glm", "zai/glm-4.6"}, Active: true},
		{Cells: []string{"glm", "zai/glm-4.6"}},
	}
	for _, width := range []int{0, 40, 120} {
		lines := RenderTable(cols, rows, width, TableOpts{})
		if len(lines) != 2 {
			t.Fatalf("width %d: got %d lines, want 2", width, len(lines))
		}
		if !strings.HasPrefix(lines[0], "▸ ") {
			t.Errorf("width %d: active row %q missing marker", width, lines[0])
		}
		if a, b := lipgloss.Width(lines[0]), lipgloss.Width(lines[1]); a != b {
			t.Errorf("width %d: active row is %d cells, inactive row is %d cells\n%q\n%q", width, a, b, lines[0], lines[1])
		}
	}
}
