package tui

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

func completerReg(entries ...SlashEntry) *SlashRegistry {
	return NewSlashRegistry(&staticProvider{entries: entries})
}

func TestSlashCompleterActivate(t *testing.T) {
	reg := completerReg(
		SlashEntry{Command: "/echo", Kind: KindSkill, Description: "echo"},
	)
	defer reg.Close()

	c := newSlashCompleter(reg)
	c.activate("/ec")

	if !c.active {
		t.Error("should be active after activate")
	}
	if len(c.visible) != 1 || c.visible[0].Command != "/echo" {
		t.Errorf("visible = %v", c.visible)
	}
}

func TestSlashCompleterDeactivate(t *testing.T) {
	reg := completerReg(SlashEntry{Command: "/x", Kind: KindSkill})
	defer reg.Close()

	c := newSlashCompleter(reg)
	c.activate("/")
	c.deactivate()

	if c.active {
		t.Error("should be inactive after deactivate")
	}
	if c.visible != nil {
		t.Error("visible should be nil after deactivate")
	}
}

func TestSlashCompleterNavigation(t *testing.T) {
	reg := completerReg(
		SlashEntry{Command: "/a", Kind: KindSkill},
		SlashEntry{Command: "/b", Kind: KindSkill},
		SlashEntry{Command: "/c", Kind: KindSkill},
	)
	defer reg.Close()

	c := newSlashCompleter(reg)
	c.activate("/")

	if c.cursor != 0 {
		t.Fatalf("initial cursor = %d, want 0", c.cursor)
	}
	c.moveDown()
	if c.cursor != 1 {
		t.Errorf("after moveDown cursor = %d, want 1", c.cursor)
	}
	c.moveUp()
	if c.cursor != 0 {
		t.Errorf("after moveUp cursor = %d, want 0", c.cursor)
	}
	// Wrap: move up from 0 → last.
	c.moveUp()
	if c.cursor != 2 {
		t.Errorf("wrap-up cursor = %d, want 2", c.cursor)
	}
	// Wrap: move down from last → 0.
	c.moveDown()
	if c.cursor != 0 {
		t.Errorf("wrap-down cursor = %d, want 0", c.cursor)
	}
}

func TestSlashCompleterSelected(t *testing.T) {
	reg := completerReg(
		SlashEntry{Command: "/first", Kind: KindSkill},
		SlashEntry{Command: "/second", Kind: KindSkill},
	)
	defer reg.Close()

	c := newSlashCompleter(reg)
	c.activate("/")

	if got := c.selected().Command; got != "/first" {
		t.Errorf("selected = %q, want /first", got)
	}
	c.moveDown()
	if got := c.selected().Command; got != "/second" {
		t.Errorf("selected after down = %q, want /second", got)
	}
}

func TestSlashCompleterView(t *testing.T) {
	entries := []SlashEntry{
		{Command: "/echo", Kind: KindMCP, Server: "everything", Description: "Echoes input"},
		{Command: "/model", Syntax: "NAME", Kind: KindBuiltin, Description: "Switch model"},
	}
	reg := completerReg(entries...)
	defer reg.Close()

	c := newSlashCompleter(reg)
	c.activate("/")

	view := c.View(120)
	if view == "" {
		t.Fatal("View should not be empty when active")
	}
	if !strings.Contains(view, "/echo") {
		t.Error("view should contain /echo")
	}
	if !strings.Contains(view, "[mcp:everything]") {
		t.Error("view should contain [mcp:everything] tag")
	}
	if !strings.Contains(view, "[builtin]") {
		t.Error("view should contain [builtin] tag")
	}
}

func TestSlashCompleterScrollIndicators(t *testing.T) {
	// Build more than completerMaxRows entries.
	var entries []SlashEntry
	for i := 0; i < completerMaxRows+3; i++ {
		entries = append(entries, SlashEntry{
			Command:     "/cmd",
			Kind:        KindSkill,
			Description: "desc",
		})
	}
	reg := completerReg(entries...)
	defer reg.Close()

	c := newSlashCompleter(reg)
	c.activate("/")

	view := c.View(120)
	if !strings.Contains(view, "↓") {
		t.Error("should show down-scroll indicator when more entries below")
	}

	// Scroll to the last entry (without wrapping).
	for i := 0; i < len(c.visible)-1; i++ {
		c.moveDown()
	}
	view = c.View(120)
	if !strings.Contains(view, "↑") {
		t.Error("should show up-scroll indicator when entries above")
	}
}

func TestSlashCompleterInactiveViewEmpty(t *testing.T) {
	reg := completerReg(SlashEntry{Command: "/x", Kind: KindSkill})
	defer reg.Close()

	c := newSlashCompleter(reg)
	if v := c.View(80); v != "" {
		t.Errorf("inactive view should be empty, got %q", v)
	}
}

func TestSlashCompleterUpdate(t *testing.T) {
	reg := completerReg(
		SlashEntry{Command: "/echo", Kind: KindSkill},
		SlashEntry{Command: "/model", Kind: KindBuiltin},
	)
	defer reg.Close()

	c := newSlashCompleter(reg)
	c.activate("/")
	if len(c.visible) != 2 {
		t.Fatalf("initial: want 2, got %d", len(c.visible))
	}

	// handleKey drives filtering through activate on every input change.
	c.activate("/ec")
	if len(c.visible) != 1 || c.visible[0].Command != "/echo" {
		t.Errorf("after activate('/ec'): visible = %v", c.visible)
	}

	// Non-slash input deactivates (handleKey calls deactivate).
	c.deactivate()
	if c.active {
		t.Error("deactivate should clear active state")
	}
}

func TestCommandWidth(t *testing.T) {
	if got := commandWidth(SlashEntry{Command: "/model", Syntax: "NAME"}); got != 11 {
		t.Fatalf("commandWidth with syntax = %d, want 11", got)
	}
	if got := commandWidth(SlashEntry{Command: "/model"}); got != 6 {
		t.Fatalf("commandWidth without syntax = %d, want 6", got)
	}
}

func TestTruncateCells(t *testing.T) {
	t.Run("within budget unchanged", func(t *testing.T) {
		for _, n := range []int{6, 7, 20} {
			if got := truncateCells("/model", n); got != "/model" {
				t.Fatalf("truncateCells(%q, %d) = %q, want unchanged", "/model", n, got)
			}
		}
	})

	t.Run("over-long ascii", func(t *testing.T) {
		got := truncateCells("abcdefghij", 5)
		if lipgloss.Width(got) != 5 {
			t.Fatalf("width = %d, want 5 (got %q)", lipgloss.Width(got), got)
		}
		if !strings.HasSuffix(got, "…") {
			t.Fatalf("got %q, want suffix …", got)
		}
	})

	t.Run("over-long multibyte", func(t *testing.T) {
		// Multibyte 1-cell runes: result is exactly n cells, on a rune boundary.
		s := "ééééééééé"
		for _, n := range []int{2, 4, 7} {
			got := truncateCells(s, n)
			if !utf8.ValidString(got) {
				t.Fatalf("n=%d: %q is not valid UTF-8", n, got)
			}
			if w := lipgloss.Width(got); w != n {
				t.Fatalf("n=%d: width = %d (%q), want %d", n, w, got, n)
			}
			if !strings.HasSuffix(got, "…") {
				t.Fatalf("n=%d: got %q, want suffix …", n, got)
			}
		}
	})

	t.Run("over-long wide runes never split", func(t *testing.T) {
		// 2-cell CJK runes cannot always fill n exactly; the budget must never
		// be exceeded and a rune must never be split.
		s := "日本語テキストです"
		for _, n := range []int{2, 3, 4, 5, 6, 7} {
			got := truncateCells(s, n)
			if !utf8.ValidString(got) {
				t.Fatalf("n=%d: %q is not valid UTF-8", n, got)
			}
			if w := lipgloss.Width(got); w > n {
				t.Fatalf("n=%d: width = %d (%q), want <= %d", n, w, got, n)
			}
			if !strings.HasSuffix(got, "…") {
				t.Fatalf("n=%d: got %q, want suffix …", n, got)
			}
			for _, r := range got {
				if r == utf8.RuneError {
					t.Fatalf("n=%d: %q contains a split rune", n, got)
				}
			}
		}
	})

	t.Run("edges", func(t *testing.T) {
		if got := truncateCells("abcdef", 0); got != "" {
			t.Fatalf("n=0 -> %q, want empty", got)
		}
		if got := truncateCells("abcdef", -3); got != "" {
			t.Fatalf("n=-3 -> %q, want empty", got)
		}
		if got := truncateCells("abcdef", 1); got != "…" {
			t.Fatalf("n=1 -> %q, want …", got)
		}
	})
}

// tagOffset returns the display-cell offset at which the kind tag begins on a
// rendered (ANSI-stripped) row, or -1 when the tag is absent.
func tagOffset(t *testing.T, row, tag string) int {
	t.Helper()
	plain := stripANSIString(row)
	i := strings.Index(plain, tag)
	if i < 0 {
		return -1
	}
	return lipgloss.Width(plain[:i])
}

func viewRows(t *testing.T, c *slashCompleter, width int) []string {
	t.Helper()
	out := strings.Split(strings.TrimRight(c.View(width), "\n"), "\n")
	if len(out) == 1 && out[0] == "" {
		t.Fatal("View returned no rows")
	}
	return out
}

func TestSlashCompleterViewKindTagColumnStable(t *testing.T) {
	reg := completerReg(
		SlashEntry{Command: "/a", Kind: KindBuiltin, Description: "alpha"},
		SlashEntry{Command: "/exit", Kind: KindBuiltin, Description: "quit"},
		SlashEntry{Command: "/blackboard", Kind: KindBuiltin, Description: "board"},
		SlashEntry{Command: "/model", Syntax: "NAME", Kind: KindBuiltin, Description: "switch"},
	)
	defer reg.Close()

	c := newSlashCompleter(reg)
	c.activate("/")

	rows := viewRows(t, c, 120)
	if len(rows) != 4 {
		t.Fatalf("want 4 rows, got %d: %q", len(rows), rows)
	}
	want := tagOffset(t, rows[0], "[builtin]")
	if want < 0 {
		t.Fatalf("row 0 has no kind tag: %q", stripANSIString(rows[0]))
	}
	for i, r := range rows {
		if got := tagOffset(t, r, "[builtin]"); got != want {
			t.Errorf("row %d tag offset = %d, want %d (row %q)", i, got, want, stripANSIString(r))
		}
	}
	// The column must be wide enough for the widest command portion, "/blackboard".
	if want < lipgloss.Width("  /blackboard  ") {
		t.Errorf("tag offset %d is narrower than the widest command portion", want)
	}
}

func TestSlashCompleterViewPadIsRegistryScoped(t *testing.T) {
	var entries []SlashEntry
	for i := 0; i < completerMaxRows; i++ {
		entries = append(entries, SlashEntry{
			Command: "/s" + string(rune('a'+i)), Kind: KindBuiltin, Description: "short",
		})
	}
	// The widest command sorts below the visible window.
	wide := "/an-extremely-long-command-name"
	entries = append(entries, SlashEntry{Command: wide, Kind: KindBuiltin, Description: "wide"})

	reg := completerReg(entries...)
	defer reg.Close()

	c := newSlashCompleter(reg)
	c.activate("/")

	rows := viewRows(t, c, 200)
	// Visible window is the first completerMaxRows rows plus a scroll indicator.
	wantOffset := lipgloss.Width("  ") + lipgloss.Width(wide) + 2
	for i := 0; i < completerMaxRows; i++ {
		if strings.Contains(stripANSIString(rows[i]), wide) {
			t.Fatalf("row %d unexpectedly shows the off-screen wide command", i)
		}
		if got := tagOffset(t, rows[i], "[builtin]"); got != wantOffset {
			t.Errorf("row %d tag offset = %d, want %d (registry-scoped pad)", i, got, wantOffset)
		}
	}
}

func TestSlashCompleterViewStyledAndUnstyledAlign(t *testing.T) {
	// Force real ANSI output: under `go test` lipgloss defaults to the Ascii
	// profile and would emit no escapes, which is exactly the case that would
	// hide a `len(styledString)` bug.
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)

	reg := completerReg(
		SlashEntry{Command: "/a", Kind: KindBuiltin, Description: "alpha"},
		SlashEntry{Command: "/model", Syntax: "NAME", Kind: KindBuiltin, Description: "switch"},
	)
	defer reg.Close()

	c := newSlashCompleter(reg)
	c.activate("/")
	// cursor is on row 0, so row 0 is styled and row 1 is not.
	rows := viewRows(t, c, 120)
	if !strings.Contains(rows[0], "\x1b[") {
		t.Fatal("row 0 should be styled (selected)")
	}
	a := tagOffset(t, rows[0], "[builtin]")
	b := tagOffset(t, rows[1], "[builtin]")
	if a < 0 || a != b {
		t.Errorf("selected tag offset = %d, unselected = %d; want equal", a, b)
	}

	// Move the cursor and re-check: the previously-unstyled row now styled.
	c.moveDown()
	rows = viewRows(t, c, 120)
	if got := tagOffset(t, rows[1], "[builtin]"); got != a {
		t.Errorf("after moveDown selected row tag offset = %d, want %d", got, a)
	}
	if got := tagOffset(t, rows[0], "[builtin]"); got != a {
		t.Errorf("after moveDown unselected row tag offset = %d, want %d", got, a)
	}
}

func TestSlashCompleterViewLongCommandKeepsKindTag(t *testing.T) {
	reg := completerReg(
		SlashEntry{
			Command:     "/an-extremely-long-command-name-that-eats-the-row",
			Kind:        KindBuiltin,
			Description: "a description that cannot possibly fit here",
		},
	)
	defer reg.Close()

	c := newSlashCompleter(reg)
	c.activate("/")

	width := 40
	row := stripANSIString(viewRows(t, c, width)[0])
	if !strings.Contains(row, "[builtin]") {
		t.Fatalf("kind tag must survive verbatim, got %q", row)
	}
	if strings.Contains(row, "a description that cannot possibly fit here") {
		t.Errorf("description should have been truncated, got %q", row)
	}
	if lipgloss.Width(row) > width {
		t.Errorf("row width = %d, want <= %d (%q)", lipgloss.Width(row), width, row)
	}
}

func TestSlashCompleterViewMultibyteDescription(t *testing.T) {
	desc := "日本語の説明テキストがここにあります"
	reg := completerReg(
		SlashEntry{Command: "/jp", Kind: KindBuiltin, Description: desc},
	)
	defer reg.Close()

	c := newSlashCompleter(reg)
	c.activate("/")

	width := 40
	row := stripANSIString(viewRows(t, c, width)[0])
	if !utf8.ValidString(row) {
		t.Fatalf("row is not valid UTF-8: %q", row)
	}
	if !strings.Contains(row, "[builtin]") {
		t.Fatalf("kind tag must survive, got %q", row)
	}
	if lipgloss.Width(row) > width {
		t.Errorf("row width = %d, want <= %d (%q)", lipgloss.Width(row), width, row)
	}
	if strings.Contains(row, desc) {
		t.Errorf("multibyte description should have been truncated, got %q", row)
	}
	if !strings.Contains(row, "…") {
		t.Errorf("truncated description should end in an ellipsis, got %q", row)
	}
}

// TestSlashCompleterViewSyntaxHighlight pins the two styling behaviors that the
// table primitive cannot express as a column style, because both apply to a
// SUBSTRING of the command cell rather than to the whole cell: the amber
// highlight on an entry's Syntax on the non-truncated path, and the selected
// row's highlight covering only the cursor+command head (not the whole line).
// A width assertion would not witness either; assert on the escape sequences.
func TestSlashCompleterViewSyntaxHighlight(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)

	reg := completerReg(
		SlashEntry{Command: "/model", Syntax: "NAME", Kind: KindBuiltin, Description: "switch"},
		SlashEntry{Command: "/exit", Kind: KindBuiltin, Description: "quit"},
	)
	defer reg.Close()

	c := newSlashCompleter(reg)
	c.activate("/")

	// Row 0 is selected: the head is wrapped in the selected style, and the
	// syntax token is separately wrapped in the syntax style OUTSIDE it.
	rows := viewRows(t, c, 120)
	if want := completerSelectedStyle.Render("▸ /model"); !strings.Contains(rows[0], want) {
		t.Errorf("selected head not styled as expected\n got %q\nwant substring %q", rows[0], want)
	}
	if want := completerSyntaxStyle.Render("NAME"); !strings.Contains(rows[0], want) {
		t.Errorf("syntax not highlighted\n got %q\nwant substring %q", rows[0], want)
	}
	// Row 1 is unselected: no selected-style wrapper on its command portion.
	if strings.Contains(rows[1], completerSelectedStyle.Render("  /exit")) {
		t.Errorf("unselected row should not carry the selected style: %q", rows[1])
	}

	// Move the cursor: the highlight follows, and the syntax highlight persists
	// on the now-unselected row.
	c.moveDown()
	rows = viewRows(t, c, 120)
	if want := completerSyntaxStyle.Render("NAME"); !strings.Contains(rows[0], want) {
		t.Errorf("syntax highlight lost when row unselected: %q", rows[0])
	}
	if want := completerSelectedStyle.Render("▸ /exit"); !strings.Contains(rows[1], want) {
		t.Errorf("selected head not styled after moveDown\n got %q\nwant substring %q", rows[1], want)
	}
}

// TestSlashCompleterPrefixCoexistenceModelAndModels asserts typing `/model`
// lists BOTH `/model` and `/models` — the shared prefix is not a collision,
// both commands are distinct and both should be visible.
func TestSlashCompleterPrefixCoexistenceModelAndModels(t *testing.T) {
	reg := NewSlashRegistry(NewBuiltinProvider())
	defer reg.Close()

	c := newSlashCompleter(reg)
	c.activate("/model")

	var sawModel, sawModels bool
	for _, e := range c.visible {
		switch e.Command {
		case "/model":
			sawModel = true
		case "/models":
			sawModels = true
		}
	}
	if !sawModel || !sawModels {
		t.Errorf("typing /model should list both /model and /models; visible = %v", c.visible)
	}
}

// TestSlashCompleterViewRowFitsWidthDespiteWideRegistryEntry pins the
// registry-wide `maxCmd` clamp. A single very wide command (an MCP tool name,
// say) must not push EVERY row past the terminal width — ShellModel composites
// the overlay through wordwrap, and an over-wide row breaks inside the pad run,
// spilling the kind tag and description onto a second line for every entry.
//
// Per the `fitline-width-invariant-hides-truncated-suffix` learning, a width
// assertion alone is not a sufficient guard: also assert the kind tag survives
// verbatim and that the tag column is identical on every row.
func TestSlashCompleterViewRowFitsWidthDespiteWideRegistryEntry(t *testing.T) {
	entries := []SlashEntry{
		{Command: "/a", Kind: KindBuiltin, Description: "alpha"},
		{Command: "/exit", Kind: KindBuiltin, Description: "quit the shell"},
		{Command: "/model", Syntax: "NAME", Kind: KindBuiltin, Description: "switch model"},
		// Far wider than any terminal width under test.
		{
			Command:     "/mcp:some-very-long-server-name/an-even-longer-tool-name",
			Kind:        KindBuiltin,
			Description: "an mcp tool",
		},
	}
	reg := completerReg(entries...)
	defer reg.Close()

	for _, width := range []int{40, 80} {
		c := newSlashCompleter(reg)
		c.activate("/")
		rows := viewRows(t, c, width)
		if len(rows) != len(entries) {
			t.Fatalf("width %d: want %d rows, got %d: %q", width, len(entries), len(rows), rows)
		}
		want := tagOffset(t, rows[0], "[builtin]")
		if want < 0 {
			t.Fatalf("width %d: row 0 has no kind tag: %q", width, stripANSIString(rows[0]))
		}
		for i, r := range rows {
			plain := stripANSIString(r)
			if got := lipgloss.Width(plain); got > width {
				t.Errorf("width %d: row %d width = %d, want <= %d (%q)", width, i, got, width, plain)
			}
			if !strings.Contains(plain, "[builtin]") {
				t.Errorf("width %d: row %d lost its kind tag: %q", width, i, plain)
			}
			if got := tagOffset(t, r, "[builtin]"); got != want {
				t.Errorf("width %d: row %d tag offset = %d, want %d (%q)", width, i, got, want, plain)
			}
		}
	}
}
