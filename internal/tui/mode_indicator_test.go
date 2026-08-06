package tui

import (
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/permissions"
)

// TestModeStatus_Token asserts the mode helper carries the canonical
// PermissionMode.String() token for each of the four modes, so the indicator
// never duplicates a hand-written mode label.
func TestModeStatus_Token(t *testing.T) {
	cases := []struct {
		mode permissions.PermissionMode
		want string
	}{
		{permissions.ModeSmart, "smart"},
		{permissions.ModeOff, "off"},
		{permissions.ModePromptAll, "prompt-all"},
		{permissions.ModeAuto, "auto"},
	}
	for _, tc := range cases {
		// classifierAvailable true so the degraded marker never confounds the
		// token assertion for auto.
		got := modeStatus(tc.mode, true)
		if !strings.Contains(got, tc.want) {
			t.Errorf("modeStatus(%v, true) = %q; want it to contain token %q", tc.mode, got, tc.want)
		}
	}
}

// TestModeStatus_DegradedMarker asserts the degraded marker appears only for
// auto-without-classifier and is absent for auto-with-classifier and for the
// other three modes (which have no degraded posture).
func TestModeStatus_DegradedMarker(t *testing.T) {
	const marker = "degraded"

	if got := modeStatus(permissions.ModeAuto, false); !strings.Contains(got, marker) {
		t.Errorf("modeStatus(auto, false) = %q; want a %q marker", got, marker)
	}

	if got := modeStatus(permissions.ModeAuto, true); strings.Contains(got, marker) {
		t.Errorf("modeStatus(auto, true) = %q; want NO %q marker", got, marker)
	}

	for _, mode := range []permissions.PermissionMode{permissions.ModeSmart, permissions.ModeOff, permissions.ModePromptAll} {
		if got := modeStatus(mode, false); strings.Contains(got, marker) {
			t.Errorf("modeStatus(%v, false) = %q; want NO %q marker for non-auto mode", mode, got, marker)
		}
	}
}

// TestModeStatus_PlainASCII guards the plain-ASCII banner convention: the
// helper output must not contain any non-ASCII glyph.
func TestModeStatus_PlainASCII(t *testing.T) {
	for _, mode := range []permissions.PermissionMode{permissions.ModeSmart, permissions.ModeOff, permissions.ModePromptAll, permissions.ModeAuto} {
		for _, avail := range []bool{true, false} {
			got := modeStatus(mode, avail)
			for i, r := range got {
				if r > 127 {
					t.Errorf("modeStatus(%v, %v) = %q; non-ASCII rune %q at byte %d", mode, avail, got, r, i)
				}
			}
		}
	}
}
