package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ethanhinson/fuse/internal/permissions"
)

// shiftTab feeds one Shift+Tab key into the model via Update and returns the
// resulting ShellModel.
func shiftTab(m ShellModel) ShellModel {
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	return next.(ShellModel)
}

// TestShiftTab_CyclesSmartAuto asserts Shift+Tab toggles the session mode
// smart -> auto -> smart on repeated presses.
func TestShiftTab_CyclesSmartAuto(t *testing.T) {
	sm := permissions.NewSessionMode(permissions.ModeSmart)
	m := sized(NewShellModel("alpha", false, "dark", testRegistry(), nil, nilBuilder, sm, true))

	m = shiftTab(m)
	if got := sm.Get(); got != permissions.ModeAuto {
		t.Fatalf("after first Shift+Tab from smart, mode = %v; want auto", got)
	}

	m = shiftTab(m)
	if got := sm.Get(); got != permissions.ModeSmart {
		t.Fatalf("after second Shift+Tab from auto, mode = %v; want smart", got)
	}
}

// TestShiftTab_LandsOnSmartFromOffAndPromptAll asserts Shift+Tab from either
// off or prompt-all lands predictably on smart, so the next press toggles
// smart<->auto.
func TestShiftTab_LandsOnSmartFromOffAndPromptAll(t *testing.T) {
	for _, start := range []permissions.PermissionMode{permissions.ModeOff, permissions.ModePromptAll} {
		sm := permissions.NewSessionMode(start)
		m := sized(NewShellModel("alpha", false, "dark", testRegistry(), nil, nilBuilder, sm, true))

		m = shiftTab(m)
		if got := sm.Get(); got != permissions.ModeSmart {
			t.Fatalf("after Shift+Tab from %v, mode = %v; want smart", start, got)
		}
		_ = m
	}
}

// TestShiftTab_IgnoredWhilePendingApproval asserts a pending approval owns the
// keyboard: Shift+Tab must not mutate the session mode while an approval is
// queued (the approval-key guard runs first in handleKey).
func TestShiftTab_IgnoredWhilePendingApproval(t *testing.T) {
	sm := permissions.NewSessionMode(permissions.ModeSmart)
	m := sized(NewShellModel("alpha", false, "dark", testRegistry(), nil, nilBuilder, sm, true))
	m.running = true

	// Enqueue an approval so the head owns the keyboard.
	respCh := make(chan approvalResponse, 1)
	next, _ := m.Update(PermissionRequestMsg{
		Request: permissions.ApprovalRequest{
			ToolName: "bash",
			Preview:  "bash: echo hi",
		},
		RespCh: respCh,
	})
	m = next.(ShellModel)
	if len(m.approvals) != 1 {
		t.Fatalf("approvals queue len = %d, want 1", len(m.approvals))
	}

	m = shiftTab(m)
	if got := sm.Get(); got != permissions.ModeSmart {
		t.Fatalf("Shift+Tab mutated mode while approval pending: mode = %v; want smart (unchanged)", got)
	}
}

// TestShiftTab_IgnoredWhileCompleterActive asserts the completer owns the
// keyboard while open: Shift+Tab must not flip the session mode with a slash
// completer overlay active (it falls through the completer navigation guard,
// which only consumes Up/Down/Esc/Enter, so the KeyShiftTab case must re-check
// completer state itself).
func TestShiftTab_IgnoredWhileCompleterActive(t *testing.T) {
	sm := permissions.NewSessionMode(permissions.ModeSmart)
	reg := registryWith(skillEntry("/code-review", "review body", "review code"))
	m := sized(NewShellModel("alpha", false, "dark", testRegistry(), reg, nilBuilder, sm, true))

	// Open the completer, as typing '/' does.
	m.input.SetValue("/")
	m.completer.activate("/")
	if !m.completer.active {
		t.Fatal("precondition: completer should be active")
	}

	m = shiftTab(m)
	if got := sm.Get(); got != permissions.ModeSmart {
		t.Fatalf("Shift+Tab mutated mode while completer active: mode = %v; want smart (unchanged)", got)
	}
}
