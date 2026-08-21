package tui

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
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
