package main

import (
	"context"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/permissions"
)

func sampleReq() permissions.ApprovalRequest {
	return permissions.ApprovalRequest{
		ToolName: "bash",
		Args:     `{"command":"rm -rf /tmp/x"}`,
		Preview:  "bash: rm -rf /tmp/x",
	}
}

// Non-TTY one-shot without --approve-all denies by default with a structured
// error message, and never silently approves.
func TestOneShotApproval_NonTTYDenies(t *testing.T) {
	fn := oneShotApprovalFunc(false /*approveAll*/, false /*isTTY*/, strings.NewReader(""), &strings.Builder{})
	approved, session, err := fn(context.Background(), sampleReq())
	if approved {
		t.Fatal("non-TTY one-shot must not auto-approve")
	}
	if session {
		t.Fatal("non-TTY one-shot must not grant session approval")
	}
	if err == nil {
		t.Fatal("non-TTY one-shot must return a structured deny error")
	}
	if !strings.Contains(err.Error(), "non-interactive") {
		t.Errorf("deny error missing structured message: %q", err.Error())
	}
}

// --approve-all restores auto-approve for scripted use, regardless of TTY.
func TestOneShotApproval_ApproveAllAutoApproves(t *testing.T) {
	fn := oneShotApprovalFunc(true /*approveAll*/, false /*isTTY*/, strings.NewReader(""), &strings.Builder{})
	approved, _, err := fn(context.Background(), sampleReq())
	if err != nil {
		t.Fatalf("--approve-all must not error: %v", err)
	}
	if !approved {
		t.Fatal("--approve-all must auto-approve")
	}
}

// The interactive TTY path prints the preview line and reads a y/N decision
// from the terminal, honoring "y" as approve.
func TestOneShotApproval_TTYPromptApproves(t *testing.T) {
	var out strings.Builder
	fn := oneShotApprovalFunc(false, true /*isTTY*/, strings.NewReader("y\n"), &out)
	approved, _, err := fn(context.Background(), sampleReq())
	if err != nil {
		t.Fatalf("TTY prompt errored: %v", err)
	}
	if !approved {
		t.Fatal("TTY prompt with 'y' must approve")
	}
	if !strings.Contains(out.String(), "bash: rm -rf /tmp/x") {
		t.Errorf("prompt did not surface the preview line: %q", out.String())
	}
}

// A bare "N" (or anything not y) at the TTY prompt denies without error.
func TestOneShotApproval_TTYPromptDenies(t *testing.T) {
	var out strings.Builder
	fn := oneShotApprovalFunc(false, true, strings.NewReader("n\n"), &out)
	approved, _, err := fn(context.Background(), sampleReq())
	if err != nil {
		t.Fatalf("TTY deny should not error: %v", err)
	}
	if approved {
		t.Fatal("TTY prompt with 'n' must deny")
	}
}

// The TTY prompt honors session_allow: "a" (always) approves and sets the
// allow-for-session flag so the gate caches it.
func TestOneShotApproval_TTYSessionAllow(t *testing.T) {
	var out strings.Builder
	fn := oneShotApprovalFunc(false, true, strings.NewReader("a\n"), &out)
	approved, session, err := fn(context.Background(), sampleReq())
	if err != nil {
		t.Fatalf("session-allow errored: %v", err)
	}
	if !approved || !session {
		t.Fatalf("'a' must approve for session: approved=%v session=%v", approved, session)
	}
}
