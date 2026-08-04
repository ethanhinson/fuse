package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ethanhinson/fuse/internal/permissions"
)

// TestPermissionRequestRendersBlock verifies that a PermissionRequestMsg causes
// an approval block to appear in the transcript.
func TestPermissionRequestRendersBlock(t *testing.T) {
	m := sized(NewShellModel("alpha", false, testRegistry(), nil, nilBuilder))
	m.running = true

	respCh := make(chan approvalResponse, 1)
	next, cmd := m.Update(PermissionRequestMsg{
		Request: permissions.ApprovalRequest{
			ToolName: "bash",
			Args:     `{"command":"rm -rf /tmp/x"}`,
			Preview:  "bash: rm -rf /tmp/x",
		},
		RespCh: respCh,
	})
	m = next.(ShellModel)

	content := strings.Join(m.lines, "\n")
	if !strings.Contains(content, "Permission required") {
		t.Error("approval block should contain 'Permission required'")
	}
	if !strings.Contains(content, "bash") {
		t.Error("approval block should contain the tool name")
	}
	if m.approval == nil {
		t.Fatal("approval state should be set after PermissionRequestMsg")
	}
	if cmd == nil {
		t.Error("expected re-armed waitForMsg cmd")
	}
}

// TestApprovalKeyY verifies that pressing 'y' sends an approved response.
func TestApprovalKeyY(t *testing.T) {
	m := sized(NewShellModel("alpha", false, testRegistry(), nil, nilBuilder))
	m.running = true

	respCh := make(chan approvalResponse, 1)
	next, _ := m.Update(PermissionRequestMsg{
		Request: permissions.ApprovalRequest{ToolName: "bash", Preview: "bash: ls"},
		RespCh:  respCh,
	})
	m = next.(ShellModel)

	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	m = next.(ShellModel)

	if m.approval != nil {
		t.Error("approval state should be cleared after responding")
	}
	select {
	case resp := <-respCh:
		if !resp.Approved {
			t.Error("expected Approved=true for 'y'")
		}
		if resp.AllowForSession {
			t.Error("expected AllowForSession=false for 'y'")
		}
	default:
		t.Fatal("no response sent to respCh after pressing 'y'")
	}
}

// TestApprovalKeyS verifies that pressing 's' sends session approval.
func TestApprovalKeyS(t *testing.T) {
	m := sized(NewShellModel("alpha", false, testRegistry(), nil, nilBuilder))
	m.running = true

	respCh := make(chan approvalResponse, 1)
	next, _ := m.Update(PermissionRequestMsg{
		Request: permissions.ApprovalRequest{ToolName: "bash", Preview: "bash: ls"},
		RespCh:  respCh,
	})
	m = next.(ShellModel)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})

	select {
	case resp := <-respCh:
		if !resp.Approved || !resp.AllowForSession {
			t.Errorf("expected Approved=true AllowForSession=true for 's', got %+v", resp)
		}
	default:
		t.Fatal("no response sent to respCh after pressing 's'")
	}
}

// TestApprovalKeyN verifies that pressing 'n' sends a denial.
func TestApprovalKeyN(t *testing.T) {
	m := sized(NewShellModel("alpha", false, testRegistry(), nil, nilBuilder))
	m.running = true

	respCh := make(chan approvalResponse, 1)
	next, _ := m.Update(PermissionRequestMsg{
		Request: permissions.ApprovalRequest{ToolName: "bash", Preview: "bash: ls"},
		RespCh:  respCh,
	})
	m = next.(ShellModel)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})

	select {
	case resp := <-respCh:
		if resp.Approved {
			t.Error("expected Approved=false for 'n'")
		}
	default:
		t.Fatal("no response sent to respCh after pressing 'n'")
	}
}

// TestApprovalViewStatus verifies that the status bar shows the approval prompt.
func TestApprovalViewStatus(t *testing.T) {
	m := sized(NewShellModel("alpha", false, testRegistry(), nil, nilBuilder))
	m.running = true

	respCh := make(chan approvalResponse, 1)
	next, _ := m.Update(PermissionRequestMsg{
		Request: permissions.ApprovalRequest{ToolName: "bash", Preview: "bash: ls"},
		RespCh:  respCh,
	})
	m = next.(ShellModel)

	view := m.View()
	if !strings.Contains(view, "Awaiting permission") {
		t.Errorf("status bar should show awaiting-permission text; got: %q", view)
	}
}
