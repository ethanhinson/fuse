package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// config_harness_test.go adds teatest frame captures for the surfaces this
// change introduced or rebuilt on the shared table/tabs primitives: the slash
// menu (table.go via slash_completer.go), the /config screen, and a tab switch
// inside it. The /models listing capture lives next door in
// models_harness_test.go (TestHarness_ModelsListing) — same harness, same
// captureFrame helper — so it is not duplicated here.
//
// Every capture goes through harness_test.go's captureOverlayFrame /
// captureModelFrame rather than a parallel harness, which is what keeps the
// four rules these captures have to obey in ONE place:
//
//  1. the frame is the FINAL MODEL's View(), never teatest's FinalOutput — the
//     last bytes on the output stream after a quit are the terminal teardown
//     frame, which is nearly empty;
//  2. lipgloss is forced to termenv.TrueColor around the View() call and
//     restored afterwards, because a non-TTY test resolves styles against the
//     Ascii profile and would capture a colourless frame;
//  3. the freeze PNG render is best-effort and skipped silently when freeze is
//     not on PATH;
//  4. artifacts land in $FUSE_SCREENSHOT_DIR when set, else t.TempDir(), so an
//     ordinary `go test ./...` leaves no files in the tree.
//
// Overlays swallow every key, including Ctrl+C, so these captures tear the
// program down with captureOverlayFrame (which calls tm.Quit()) rather than
// sending a keystroke that can never be delivered.

// typeNoSubmit sends s as individual key presses without a trailing Enter, so
// the completer overlay stays open and can be captured mid-flight.
func typeNoSubmit(h *harness, s string) {
	h.t.Helper()
	for _, r := range s {
		h.tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
}

// openConfigHarness boots the shared models harness, opens /config through the
// real typed keystroke path, and waits for the overlay to paint.
func openConfigHarness(t *testing.T) *harness {
	t.Helper()
	h := modelsHarness(t, richModelRegistry())
	// The completer is active on "/config"; Enter dispatches the selected
	// builtin entry, which is the same path a user's keystrokes take.
	h.typeAndSubmit("/config")
	h.waitForOutput("Config", 2*time.Second)
	return h
}

// TestHarnessCapture_SlashMenu captures the slash-command menu — the completer
// overlay now rendered through the shared table primitive.
func TestHarnessCapture_SlashMenu(t *testing.T) {
	h := modelsHarness(t, richModelRegistry())

	typeNoSubmit(h, "/")
	h.waitForOutput("/models", 2*time.Second)

	frame := captureOverlayFrame(t, h.tm, "slash-menu")
	for _, want := range []string{"/models", "/config"} {
		if !strings.Contains(frame, want) {
			t.Errorf("slash-menu frame missing %q; frame:\n%s", want, frame)
		}
	}
}

// TestHarnessCapture_ConfigScreen captures /config on its default (Models) tab,
// driven end-to-end through the live program.
func TestHarnessCapture_ConfigScreen(t *testing.T) {
	h := openConfigHarness(t)

	frame := captureOverlayFrame(t, h.tm, "config-screen")
	// The tab bar plus the Models pane's own content: an alias column fed by
	// the shared models editor state.
	for _, want := range []string{"Config", "Models", "Permissions", "MCP", "alias", "deepseek-flash"} {
		if !strings.Contains(frame, want) {
			t.Errorf("config frame missing %q; frame:\n%s", want, frame)
		}
	}
}

// TestHarnessCapture_ConfigTabSwitch captures /config AFTER a tab keystroke, so
// the artifact shows the second pane and the assertion proves the switch really
// happened through the shell's key path (pane content, not just the tab bar,
// which lists every title regardless of which one is active).
func TestHarnessCapture_ConfigTabSwitch(t *testing.T) {
	h := openConfigHarness(t)

	// The Models pane is showing (TestHarnessCapture_ConfigScreen captures that
	// state). Note we do NOT wait on the output stream again before sending the
	// key: teatest's Output() is a consumed stream, and with no further input
	// the program emits no new frame, so a second wait on already-drained
	// content would block until timeout.
	h.tm.Send(tea.KeyMsg{Type: tea.KeyTab})
	h.waitForOutput("permission mode", 2*time.Second)

	frame := captureOverlayFrame(t, h.tm, "config-tab-switch")
	for _, want := range []string{"Permissions", "permission mode", "prompt-all", "read-only"} {
		if !strings.Contains(frame, want) {
			t.Errorf("tab-switch frame missing %q; frame:\n%s", want, frame)
		}
	}
	// The Models pane must be gone — otherwise the "switch" only repainted the
	// tab bar and left the old pane underneath.
	if strings.Contains(frame, "d delete") {
		t.Errorf("Models pane still rendered after switching tabs; frame:\n%s", frame)
	}
}
