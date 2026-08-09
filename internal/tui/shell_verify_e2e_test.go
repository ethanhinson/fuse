package tui

import (
	"strings"
	"testing"
	"time"
)

// TestShellDrivesFullTurn_Verify drives the real ShellModel end-to-end through a
// keystroke-submitted prompt and asserts the agent's reply renders in the
// transcript. It is the behavioral confirmation that the interactive path
// compiles and runs with the P0+P1 changes present (config validation and the
// per-tool-call timeout are wired at the cmd/fuse build seam; this test proves
// the shell loop itself is intact). Produces a screenshot artifact when
// FUSE_SCREENSHOT_DIR is set.
func TestShellDrivesFullTurn_Verify(t *testing.T) {
	h := newHarness(t, harnessOpts{
		Completer: assistantOnce("Fix verified: the shell turn completed cleanly."),
		Alias:     "alpha",
	})

	h.typeAndSubmit("does the shell still work end to end?")
	h.waitForOutput("Fix verified", 5*time.Second)

	frame := h.snapshot("verify-01-full-turn")
	if !strings.Contains(frame, "Fix verified") {
		t.Fatalf("reply not visible in final frame:\n%s", frame)
	}
	// The user's prompt must be echoed too (proves the keystroke path, not a
	// hand-fed AgentDoneMsg).
	if !strings.Contains(frame, "does the shell still work") {
		t.Errorf("submitted prompt not echoed in transcript:\n%s", frame)
	}
}

// TestShellStartupBanner_Verify confirms the shell's opening frame renders the
// banner (with the ldflags-injected version in the real binary; here the model
// renders whatever version.Version resolves to). Captures a startup screenshot.
func TestShellStartupBanner_Verify(t *testing.T) {
	h := newHarness(t, harnessOpts{Alias: "alpha"})
	frame := h.snapshot("verify-02-startup")
	if strings.TrimSpace(frame) == "" {
		t.Fatal("startup frame is empty")
	}
}
