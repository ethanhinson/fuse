package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// TestConfigIsStickyFullScreen: /config takes over the whole terminal (no
// transcript, input or status bar) and only Esc closes it.
func TestConfigIsStickyFullScreen(t *testing.T) {
	m := openConfigVia(t, configShell(t, configRegistry()))
	lines := strings.Split(m.View(), "\n")
	if want := m.vp.Height + chromeHeight; len(lines) != want {
		t.Fatalf("screen is %d rows, want %d (full terminal)", len(lines), want)
	}
	for i, l := range lines {
		if w := lipgloss.Width(l); w > m.vp.Width {
			t.Errorf("row %d is %d cells, width %d", i, w, m.vp.Width)
		}
	}
	if last := plainStr(lines[len(lines)-1]); !strings.Contains(last, "Esc close") {
		t.Errorf("hint not pinned to the last row: %q", last)
	}
	if strings.Contains(plainStr(m.View()), "mode:") {
		t.Error("status bar leaked into the full-screen view")
	}
	// Sticky: ordinary keys do not dismiss it.
	m = configKey(m, runeKey('x'))
	if m.config == nil {
		t.Fatal("a stray key closed /config")
	}
	m = configKey(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.config != nil {
		t.Fatal("esc did not close /config")
	}
}

func TestConfigArrowKeysSwitchTabs(t *testing.T) {
	m := openConfigVia(t, configShell(t, configRegistry()))
	m = configKey(m, tea.KeyMsg{Type: tea.KeyRight})
	if got := m.config.tabs.Active(); got != configTabPermissions {
		t.Fatalf("right: active=%d", got)
	}
	m = configKey(m, tea.KeyMsg{Type: tea.KeyLeft})
	m = configKey(m, tea.KeyMsg{Type: tea.KeyLeft})
	if got := m.config.tabs.Active(); got != configTabMCP {
		t.Fatalf("left should wrap: active=%d", got)
	}
}

// TestModelsPickerSelectsWithKeyboard: bare /models opens the pane UI, arrows
// move, Enter switches the active model and closes the screen.
func TestModelsPickerSelectsWithKeyboard(t *testing.T) {
	m := configShell(t, configRegistry())
	next, _ := m.handleSlash("/models")
	m = next.(ShellModel)
	if m.modelsEdit == nil || !m.screenOpen() {
		t.Fatal("/models should open the full-screen picker")
	}
	if !strings.Contains(plainStr(m.View()), "(active)") {
		t.Error("active model not marked")
	}
	m = configKey(m, tea.KeyMsg{Type: tea.KeyDown})
	m = configKey(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.alias != "beta" {
		t.Fatalf("alias = %q, want beta", m.alias)
	}
	if m.modelsEdit != nil {
		t.Error("picker should close after selecting")
	}
}
