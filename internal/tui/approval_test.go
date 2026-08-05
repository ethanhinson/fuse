package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/permissions"
)

// TestPermissionRequestShowsOverlayThenCompactsOnAnswer: the pending modal
// renders as a live overlay (not in the transcript); answering dismisses it
// and leaves a compact timestamped decision record in the transcript.
func TestPermissionRequestShowsOverlayThenCompactsOnAnswer(t *testing.T) {
	m := sized(NewShellModel("alpha", false, "dark", testRegistry(), nil, nilBuilder))
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
	if cmd != nil {
		t.Error("channel messages should not return a subscription cmd")
	}
	if len(m.approvals) != 1 {
		t.Fatalf("approvals queue len = %d, want 1", len(m.approvals))
	}

	// Pending: modal visible in the VIEW, absent from the transcript lines.
	view := ansiRE.ReplaceAllString(m.View(), "")
	if !strings.Contains(view, "Permission required") {
		t.Error("pending approval should render as a view overlay")
	}
	if strings.Contains(plainLines(m), "Permission required") {
		t.Error("modal must not be written into the transcript")
	}

	// Answer with session-allow: modal gone, compact record present.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	m = next.(ShellModel)
	view = ansiRE.ReplaceAllString(m.View(), "")
	if strings.Contains(view, "Permission required") {
		t.Error("modal should disappear once answered")
	}
	rec := plainLines(m)
	if !strings.Contains(rec, "bash: rm -rf /tmp/x") || !strings.Contains(rec, "allowed for session") {
		t.Errorf("compact decision record missing: %q", rec)
	}
	if len(m.approvalLog) != 1 {
		t.Errorf("approval log len = %d, want 1", len(m.approvalLog))
	}
}

// TestApprovalsCommandListsDecisions: /approvals recalls the session log.
func TestApprovalsCommandListsDecisions(t *testing.T) {
	m := sized(NewShellModel("alpha", false, "dark", testRegistry(), nil, nilBuilder))
	m.running = true
	respCh := make(chan approvalResponse, 1)
	next, _ := m.Update(PermissionRequestMsg{
		Request: permissions.ApprovalRequest{ToolName: "bash", Preview: "bash: make deploy"},
		RespCh:  respCh,
	})
	m = next.(ShellModel)
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	m = next.(ShellModel)
	m.running = false

	m = typeLine(m, "/approvals")
	m, _ = enter(m)
	out := plainLines(m)
	if !strings.Contains(out, "Permission decisions (1)") {
		t.Errorf("/approvals header missing: %q", out)
	}
	if !strings.Contains(out, "make deploy") || !strings.Contains(out, "denied") {
		t.Errorf("/approvals should list the denial: %q", out)
	}
}

// TestApprovalKeyY verifies that pressing 'y' sends an approved response.
func TestApprovalKeyY(t *testing.T) {
	m := sized(NewShellModel("alpha", false, "dark", testRegistry(), nil, nilBuilder))
	m.running = true

	respCh := make(chan approvalResponse, 1)
	next, _ := m.Update(PermissionRequestMsg{
		Request: permissions.ApprovalRequest{ToolName: "bash", Preview: "bash: ls"},
		RespCh:  respCh,
	})
	m = next.(ShellModel)

	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	m = next.(ShellModel)

	if len(m.approvals) != 0 {
		t.Error("approvals queue should be empty after responding")
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
	m := sized(NewShellModel("alpha", false, "dark", testRegistry(), nil, nilBuilder))
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
	m := sized(NewShellModel("alpha", false, "dark", testRegistry(), nil, nilBuilder))
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

// TestConcurrentApprovalsQueueFIFO verifies that two requests arriving before
// any keypress are BOTH answered, in order — the historical bug overwrote the
// first request's RespCh, deadlocking its gate goroutine forever.
func TestConcurrentApprovalsQueueFIFO(t *testing.T) {
	m := sized(NewShellModel("alpha", false, "dark", testRegistry(), nil, nilBuilder))
	m.running = true

	ch1 := make(chan approvalResponse, 1)
	ch2 := make(chan approvalResponse, 1)
	next, _ := m.Update(PermissionRequestMsg{
		Request: permissions.ApprovalRequest{ToolName: "bash", Preview: "[worker-1] bash: ls"},
		RespCh:  ch1,
	})
	m = next.(ShellModel)
	next, _ = m.Update(PermissionRequestMsg{
		Request: permissions.ApprovalRequest{ToolName: "bash", Preview: "[worker-2] bash: pwd"},
		RespCh:  ch2,
	})
	m = next.(ShellModel)

	if len(m.approvals) != 2 {
		t.Fatalf("queue len = %d, want 2", len(m.approvals))
	}

	// First keypress answers the FIRST request only.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	m = next.(ShellModel)
	select {
	case resp := <-ch1:
		if !resp.Approved {
			t.Error("first request should be approved")
		}
	default:
		t.Fatal("first RespCh not answered — queue orphaned it")
	}
	select {
	case <-ch2:
		t.Fatal("second request answered prematurely")
	default:
	}

	// Second keypress answers the second.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	m = next.(ShellModel)
	select {
	case resp := <-ch2:
		if resp.Approved {
			t.Error("second request should be denied")
		}
	default:
		t.Fatal("second RespCh not answered")
	}
	if len(m.approvals) != 0 {
		t.Errorf("queue should be empty, len = %d", len(m.approvals))
	}
}

// TestApprovalSurvivesOverlayExit verifies a pending approval is not dropped
// when the user exits the agents overlay — the historical bug nil'd the overlay
// model along with the request, deadlocking the gate.
func TestApprovalSurvivesOverlayExit(t *testing.T) {
	m := sized(NewShellModel("alpha", false, "dark", testRegistry(), nil, nilBuilder))
	m = m.WithTree(agent.NewAgentTree("root", "alpha"))
	m.running = true

	// Enter the agents overlay.
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = next.(ShellModel)
	if !m.agentsActive {
		t.Fatal("overlay should be active after Tab")
	}

	// Approval arrives while the overlay is open.
	respCh := make(chan approvalResponse, 1)
	next, _ = m.Update(PermissionRequestMsg{
		Request: permissions.ApprovalRequest{ToolName: "bash", Preview: "bash: ls"},
		RespCh:  respCh,
	})
	m = next.(ShellModel)
	if len(m.approvals) != 1 {
		t.Fatalf("queue len = %d, want 1 while overlay active", len(m.approvals))
	}

	// Answer it from within the overlay; the shell-owned queue handles the key.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	m = next.(ShellModel)
	select {
	case resp := <-respCh:
		if !resp.Approved {
			t.Error("expected approval")
		}
	default:
		t.Fatal("RespCh not answered while overlay active")
	}
}

// TestAgentEventsFlowDuringOverlay verifies channel messages fall through to
// the shared handler while the agents overlay is open — before Program.Send,
// the overlay branch drained assistant text without appending it, so prose
// arriving mid-overlay was lost from the transcript.
func TestAgentEventsFlowDuringOverlay(t *testing.T) {
	m := sized(NewShellModel("alpha", false, "dark", testRegistry(), nil, nilBuilder))
	m = m.WithTree(agent.NewAgentTree("root", "alpha"))
	m.running = true

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = next.(ShellModel)
	if !m.agentsActive {
		t.Fatal("overlay should be active after Tab")
	}

	next, _ = m.Update(AssistantMsg{Text: "prose arriving mid-overlay"})
	m = next.(ShellModel)
	if !m.agentsActive {
		t.Error("overlay should remain active across channel messages")
	}
	if !strings.Contains(plainLines(m), "prose arriving mid-overlay") {
		t.Error("assistant text arriving during overlay must land in the transcript")
	}

	// Turn end while overlay open: state updates, overlay stays.
	next, _ = m.Update(AgentDoneMsg{})
	m = next.(ShellModel)
	if m.running {
		t.Error("running should clear on AgentDoneMsg during overlay")
	}
	if !m.agentsActive {
		t.Error("overlay should survive AgentDoneMsg")
	}
}

// TestEscDeniesApproval verifies the Esc key denies (bubbletea names it "esc").
func TestEscDeniesApproval(t *testing.T) {
	m := sized(NewShellModel("alpha", false, "dark", testRegistry(), nil, nilBuilder))
	m.running = true

	respCh := make(chan approvalResponse, 1)
	next, _ := m.Update(PermissionRequestMsg{
		Request: permissions.ApprovalRequest{ToolName: "bash", Preview: "bash: ls"},
		RespCh:  respCh,
	})
	m = next.(ShellModel)
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = next.(ShellModel)

	select {
	case resp := <-respCh:
		if resp.Approved {
			t.Error("Esc should deny")
		}
	default:
		t.Fatal("no response sent after Esc")
	}
}

// TestTurnEndDrainsApprovals verifies queued approvals are denied when the
// turn ends, so no gate goroutine is left blocked forever.
func TestTurnEndDrainsApprovals(t *testing.T) {
	m := sized(NewShellModel("alpha", false, "dark", testRegistry(), nil, nilBuilder))
	m.running = true

	respCh := make(chan approvalResponse, 1)
	next, _ := m.Update(PermissionRequestMsg{
		Request: permissions.ApprovalRequest{ToolName: "bash", Preview: "bash: ls"},
		RespCh:  respCh,
	})
	m = next.(ShellModel)
	next, _ = m.Update(AgentDoneMsg{})
	m = next.(ShellModel)

	select {
	case resp := <-respCh:
		if resp.Approved {
			t.Error("drained approval should be denied")
		}
	default:
		t.Fatal("turn end should answer queued approvals")
	}
	if len(m.approvals) != 0 {
		t.Errorf("queue should be drained, len = %d", len(m.approvals))
	}
}

// TestApprovalViewStatus verifies that the status bar shows the approval prompt.
func TestApprovalViewStatus(t *testing.T) {
	m := sized(NewShellModel("alpha", false, "dark", testRegistry(), nil, nilBuilder))
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
