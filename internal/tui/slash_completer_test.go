package tui

import (
	"strings"
	"testing"
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
