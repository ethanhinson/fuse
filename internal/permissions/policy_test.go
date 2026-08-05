package permissions

import "testing"

func TestParseMode(t *testing.T) {
	cases := []struct {
		in   string
		want PermissionMode
	}{
		{"off", ModeOff},
		{"prompt-all", ModePromptAll},
		{"auto", ModeAuto},
		{"smart", ModeSmart},
		{"", ModeSmart},
	}
	for _, tc := range cases {
		if got := ParseMode(tc.in); got != tc.want {
			t.Errorf("ParseMode(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
