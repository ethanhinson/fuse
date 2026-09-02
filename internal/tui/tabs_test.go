package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// key builds the tea.KeyMsg a terminal would deliver for the named key.
func tabKey(s string) tea.KeyMsg {
	switch s {
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "shift+tab":
		return tea.KeyMsg{Type: tea.KeyShiftTab}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
}

// countingPanes builds n panes whose Key funcs record which pane was called.
func countingPanes(titles []string, calls []int) []Pane {
	panes := make([]Pane, len(titles))
	for i := range titles {
		i := i
		panes[i] = Pane{
			Title: titles[i],
			View:  func(w, h int) string { return titles[i] },
			Key: func(tea.KeyMsg) (bool, tea.Cmd) {
				calls[i]++
				return true, nil
			},
		}
	}
	return panes
}

func TestTabsCyclingWraps(t *testing.T) {
	calls := make([]int, 3)
	tabs := NewTabs(countingPanes([]string{"Models", "Permissions", "MCP"}, calls)...)

	for i, want := range []int{1, 2, 0, 1} {
		handled, _ := tabs.Update(tabKey("tab"))
		if !handled {
			t.Fatalf("step %d: tab not handled", i)
		}
		if got := tabs.Active(); got != want {
			t.Fatalf("step %d: after tab active=%d want %d", i, got, want)
		}
	}

	tabs.SetActive(0)
	for i, want := range []int{2, 1, 0, 2} {
		handled, _ := tabs.Update(tabKey("shift+tab"))
		if !handled {
			t.Fatalf("step %d: shift+tab not handled", i)
		}
		if got := tabs.Active(); got != want {
			t.Fatalf("step %d: after shift+tab active=%d want %d", i, got, want)
		}
	}

	total := 0
	for _, c := range calls {
		total += c
	}
	if total != 0 {
		t.Fatalf("switch keys leaked to pane Key funcs: %v", calls)
	}
}

func TestTabsDirectIndexKeys(t *testing.T) {
	calls := make([]int, 3)
	tabs := NewTabs(countingPanes([]string{"Models", "Permissions", "MCP"}, calls)...)

	for _, tc := range []struct {
		k    string
		want int
	}{{"2", 1}, {"3", 2}, {"1", 0}} {
		handled, _ := tabs.Update(tabKey(tc.k))
		if !handled {
			t.Fatalf("%q not handled", tc.k)
		}
		if got := tabs.Active(); got != tc.want {
			t.Fatalf("%q selected %d, want %d", tc.k, got, tc.want)
		}
	}

	// Out-of-range index must not panic and must not move the selection.
	tabs.SetActive(1)
	if _, _ = tabs.Update(tabKey("9")); tabs.Active() != 1 {
		t.Fatalf("out-of-range digit moved active to %d", tabs.Active())
	}
	tabs.SetActive(7)
	if tabs.Active() != 1 {
		t.Fatalf("SetActive(7) moved active to %d, want 1", tabs.Active())
	}
	tabs.SetActive(-1)
	if tabs.Active() != 1 {
		t.Fatalf("SetActive(-1) moved active to %d, want 1", tabs.Active())
	}
}

func TestTabsDelegatesToActivePaneOnly(t *testing.T) {
	calls := make([]int, 3)
	tabs := NewTabs(countingPanes([]string{"Models", "Permissions", "MCP"}, calls)...)
	tabs.SetActive(1)

	handled, _ := tabs.Update(tabKey("x"))
	if !handled {
		t.Fatal("active pane reported the key as unhandled")
	}
	if calls[1] != 1 {
		t.Fatalf("active pane called %d times, want 1", calls[1])
	}
	if calls[0] != 0 || calls[2] != 0 {
		t.Fatalf("neighbour panes were called: %v", calls)
	}
}

func TestTabsNilKeyPaneIsInert(t *testing.T) {
	tabs := NewTabs(
		Pane{Title: "Inert", View: func(w, h int) string { return "" }},
		Pane{Title: "Other", View: func(w, h int) string { return "" }},
	)
	handled, cmd := tabs.Update(tabKey("x"))
	if handled {
		t.Fatal("nil-Key pane reported the key as handled")
	}
	if cmd != nil {
		t.Fatal("nil-Key pane returned a command")
	}
}

func TestTabsEscIsUnhandledSoHostCloses(t *testing.T) {
	calls := make([]int, 2)
	tabs := NewTabs(countingPanes([]string{"Models", "MCP"}, calls)...)
	handled, cmd := tabs.Update(tabKey("esc"))
	if handled {
		t.Fatal("esc was handled by Tabs; the hosting overlay can no longer close")
	}
	if cmd != nil {
		t.Fatal("esc produced a command")
	}
	if calls[0] != 0 {
		t.Fatal("esc was delegated to the active pane; a pane could swallow the close")
	}
}

func TestTabsViewFitsWidth(t *testing.T) {
	long := strings.Repeat("Permissions and policy configuration ", 4)
	panes := []Pane{
		{Title: "Models", View: func(w, h int) string { return strings.Repeat("model-alias-row ", 12) }},
		{Title: long, View: func(w, h int) string { return long + "\n" + long }},
		{Title: "MCP servers configured for this session", View: func(w, h int) string { return "" }},
	}
	tabs := NewTabs(panes...)

	for _, width := range []int{40, 60, 80, 100, 120} {
		for active := 0; active < len(panes); active++ {
			tabs.SetActive(active)
			out := tabs.View(width, 10)
			for i, line := range strings.Split(out, "\n") {
				if w := lipgloss.Width(line); w > width {
					t.Errorf("width=%d active=%d line %d is %d cells: %q", width, active, i, w, line)
				}
			}
		}
	}
}

func TestTabsViewShowsActiveTitleAndPane(t *testing.T) {
	tabs := NewTabs(
		Pane{Title: "Models", View: func(w, h int) string { return "MODELS-BODY" }},
		Pane{Title: "MCP", View: func(w, h int) string { return "MCP-BODY" }},
	)
	out := tabs.View(80, 6)
	if !strings.Contains(out, "Models") || !strings.Contains(out, "MCP") {
		t.Fatalf("tab bar missing a title:\n%s", out)
	}
	if !strings.Contains(out, "MODELS-BODY") {
		t.Fatalf("active pane body missing:\n%s", out)
	}
	if strings.Contains(out, "MCP-BODY") {
		t.Fatalf("inactive pane body rendered:\n%s", out)
	}
	tabs.Next()
	if out = tabs.View(80, 6); !strings.Contains(out, "MCP-BODY") {
		t.Fatalf("after Next the MCP body should render:\n%s", out)
	}
}

func TestTabsEmptyIsSafe(t *testing.T) {
	var tabs Tabs
	if handled, _ := tabs.Update(tabKey("tab")); handled {
		t.Fatal("empty Tabs handled tab")
	}
	tabs.Next()
	tabs.Prev()
	if got := tabs.View(40, 4); got != "" {
		t.Fatalf("empty Tabs rendered %q", got)
	}
}
