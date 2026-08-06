package permissions

import "testing"

// segsFor drives the real splitSegments so rule tests exercise the same
// parser the gate will use, rather than hand-fabricating segments.
func segsFor(t *testing.T, cmd string) []Segment {
	t.Helper()
	segs, err := splitSegments(cmd)
	if err != nil {
		t.Fatalf("splitSegments(%q) unexpected error: %v", cmd, err)
	}
	return segs
}

func TestEvalRules_Precedence(t *testing.T) {
	cases := []struct {
		desc         string
		cmd          string
		auto         AutoConfig
		autoApprove  []string
		alwaysPrompt []string
		want         Verdict
	}{
		{
			desc:        "allow rule does not approve compound with dangerous segment",
			cmd:         "git status && rm -rf ~",
			autoApprove: []string{"bash:git *"},
			want:        VerdictDeny, // deny wins on segment 2 (rm)
		},
		{
			desc:        "whole-string allow never overrides a segment deny",
			cmd:         "echo safe && rm file",
			autoApprove: []string{"bash:*"}, // would match the whole string
			want:        VerdictDeny,
		},
		{
			desc: "unmatched command defaults to ask, never allow",
			cmd:  "frobnicate --wibble",
			want: VerdictAsk,
		},
		{
			desc:        "single allowed read-only-shaped command under allow rule => allow",
			cmd:         "git status",
			autoApprove: []string{"bash:git *"},
			want:        VerdictAllow,
		},
		{
			desc:        "ask pattern from cfg.Ask forces ask over an allow rule",
			cmd:         "git status",
			auto:        AutoConfig{Ask: []string{"bash:git status"}},
			autoApprove: []string{"bash:git *"},
			want:        VerdictAsk,
		},
		{
			desc:         "ask pattern from alwaysPrompt forces ask over an allow rule",
			cmd:          "git status",
			autoApprove:  []string{"bash:git *"},
			alwaysPrompt: []string{"bash:git status"},
			want:         VerdictAsk,
		},
		{
			desc:        "deny wins even when an ask pattern also matches",
			cmd:         "rm -rf ~",
			auto:        AutoConfig{Ask: []string{"bash:rm *"}},
			autoApprove: []string{"bash:*"},
			want:        VerdictDeny,
		},
		{
			desc: "git push is deny even without config (subcommand-qualified)",
			cmd:  "git push origin main",
			want: VerdictDeny,
		},
		{
			desc: "bare curl is a coarse deny (network egress)",
			cmd:  "curl http://example.com",
			want: VerdictDeny,
		},
		{
			desc:        "extra cfg.Deny pattern denies a segment",
			cmd:         "echo hi && npmpublish",
			auto:        AutoConfig{Deny: []string{"bash:npmpublish"}},
			autoApprove: []string{"bash:*"},
			want:        VerdictDeny,
		},
		{
			desc:        "every segment allowed => allow",
			cmd:         "git status && git log",
			autoApprove: []string{"bash:git *"},
			want:        VerdictAllow,
		},
	}

	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			segs := segsFor(t, tc.cmd)
			got := evalRules(segs, tc.auto, tc.autoApprove, tc.alwaysPrompt)
			if got != tc.want {
				t.Errorf("evalRules(%q) = %v, want %v", tc.cmd, got, tc.want)
			}
		})
	}
}

// TestEvalRules_DenylistWrapping proves that sh -c "rm x" — whose inner rm
// segment is surfaced by splitSegments — still denies, driven end-to-end from
// the command string.
func TestEvalRules_DenylistWrapping(t *testing.T) {
	segs := segsFor(t, `sh -c "rm x"`)
	got := evalRules(segs, AutoConfig{}, []string{"bash:*"}, nil)
	if got != VerdictDeny {
		t.Fatalf("evalRules(sh -c \"rm x\") = %v, want VerdictDeny", got)
	}
}

func TestVerdictString(t *testing.T) {
	cases := []struct {
		v    Verdict
		want string
	}{
		{VerdictAllow, "allow"},
		{VerdictAsk, "ask"},
		{VerdictDeny, "deny"},
	}
	for _, tc := range cases {
		if got := tc.v.String(); got != tc.want {
			t.Errorf("Verdict(%d).String() = %q, want %q", tc.v, got, tc.want)
		}
	}
}
