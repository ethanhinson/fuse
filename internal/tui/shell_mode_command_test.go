package tui

import (
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/permissions"
)

// modeModel builds a sized ShellModel whose session mode starts at the given
// mode, for driving /mode through handleSlash.
func modeModel(t *testing.T, start permissions.PermissionMode) (ShellModel, *permissions.SessionMode) {
	t.Helper()
	sm := permissions.NewSessionMode(start)
	m := sized(NewShellModel("alpha", false, "dark", testRegistry(), nil, nilBuilder, sm, true))
	m.lines = nil // drop banner/welcome lines so we inspect only what /mode appends.
	return m, sm
}

// TestModeCommand_BarePrintsActiveAndOptions asserts a bare `/mode` names the
// current mode and lists all four options with a usage hint, changing nothing.
func TestModeCommand_BarePrintsActiveAndOptions(t *testing.T) {
	m, sm := modeModel(t, permissions.ModeAuto)
	next, _ := m.handleSlash("/mode")
	out := plainLines(next.(ShellModel))

	if !strings.Contains(out, "auto") {
		t.Errorf("bare /mode should name the current mode (auto); got:\n%s", out)
	}
	for _, opt := range []string{"smart", "auto", "prompt-all", "off"} {
		if !strings.Contains(out, opt) {
			t.Errorf("bare /mode should list option %q; got:\n%s", opt, out)
		}
	}
	if !strings.Contains(out, "/mode") {
		t.Errorf("bare /mode should print a usage hint mentioning /mode; got:\n%s", out)
	}
	if got := sm.Get(); got != permissions.ModeAuto {
		t.Errorf("bare /mode must not change the mode; got %v, want auto", got)
	}
}

// TestModeCommand_SetsEachValidMode asserts `/mode <name>` sets the session mode
// for every one of the four valid tokens and echoes the new mode.
func TestModeCommand_SetsEachValidMode(t *testing.T) {
	cases := []struct {
		name string
		want permissions.PermissionMode
	}{
		{"auto", permissions.ModeAuto},
		{"prompt-all", permissions.ModePromptAll},
		{"off", permissions.ModeOff},
		{"smart", permissions.ModeSmart},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Start from a mode different than the target so a set is observable.
			m, sm := modeModel(t, permissions.ModePromptAll)
			if tc.want == permissions.ModePromptAll {
				m, sm = modeModel(t, permissions.ModeSmart)
			}
			next, _ := m.handleSlash("/mode " + tc.name)
			if got := sm.Get(); got != tc.want {
				t.Fatalf("/mode %s: session mode = %v, want %v", tc.name, got, tc.want)
			}
			out := plainLines(next.(ShellModel))
			if !strings.Contains(out, tc.name) {
				t.Errorf("/mode %s should echo the new mode; got:\n%s", tc.name, out)
			}
		})
	}
}

// TestModeCommand_UnknownNameLeavesModeUnchanged asserts `/mode bogus` prints a
// usage/error line and does not change the session mode. This guards the strict
// round-trip validation (ParseMode defaults unknown tokens to smart, which must
// NOT be trusted as a silent switch to smart).
func TestModeCommand_UnknownNameLeavesModeUnchanged(t *testing.T) {
	m, sm := modeModel(t, permissions.ModeAuto)
	next, _ := m.handleSlash("/mode bogus")
	if got := sm.Get(); got != permissions.ModeAuto {
		t.Fatalf("/mode bogus must not change the mode; got %v, want auto", got)
	}
	out := plainLines(next.(ShellModel))
	if !strings.Contains(out, "/mode") {
		t.Errorf("/mode bogus should print a usage/error line; got:\n%s", out)
	}
}

// TestModeCommand_RegisteredInBuiltinProvider asserts /mode is present in the
// built-in command list (so it appears in the completer).
func TestModeCommand_RegisteredInBuiltinProvider(t *testing.T) {
	for _, e := range NewBuiltinProvider().Commands() {
		if e.Command == "/mode" {
			return
		}
	}
	t.Fatal("/mode not registered in NewBuiltinProvider().Commands()")
}
