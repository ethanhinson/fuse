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
			cmd:         "echo safe && sudoedit /etc/hosts",
			autoApprove: []string{"bash:*"}, // would match the whole string
			want:        VerdictDeny,
		},
		{
			desc:        "sudoedit is a dangerous name — denied even under a broad allow",
			cmd:         "sudoedit /etc/hosts",
			autoApprove: []string{"bash:*"},
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
			desc: "git push without allow_push falls through to ask (egress/classifier routing, 0068)",
			cmd:  "git push origin main",
			want: VerdictAsk,
		},
		{
			desc: "git push with allow_push opt-in is a deterministic allow (0068)",
			cmd:  "git push origin main",
			auto: AutoConfig{AllowPush: true},
			want: VerdictAllow,
		},
		{
			desc: "config deny still beats allow_push",
			cmd:  "git push origin main",
			auto: AutoConfig{AllowPush: true, Deny: []string{"bash:git push*"}},
			want: VerdictDeny,
		},
		{
			desc:         "always_prompt still beats allow_push",
			cmd:          "git push origin main",
			auto:         AutoConfig{AllowPush: true},
			alwaysPrompt: []string{"bash:git push*"},
			want:         VerdictAsk,
		},
		{
			desc: "bare curl is no longer a rules deny — falls through to the heuristic/classifier (0068)",
			cmd:  "curl http://example.com",
			want: VerdictAsk,
		},
		{
			desc: "mkfs variant is catastrophic (prefix match)",
			cmd:  "mkfs.ext4 /dev/sda1",
			want: VerdictDeny,
		},
		{
			desc: "dd onto a raw device is catastrophic",
			cmd:  "dd if=image.img of=/dev/disk0",
			want: VerdictDeny,
		},
		{
			desc: "rm -rf / is catastrophic",
			cmd:  "rm -rf /",
			want: VerdictDeny,
		},
		{
			desc: "plain rm falls through to the heuristic layer (in-workspace scoping)",
			cmd:  "rm build/output.txt",
			want: VerdictAsk, // rules layer defers; heuristic allows in-workspace
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
			got := evalRules(segs, tc.auto, tc.autoApprove, tc.alwaysPrompt, "")
			if got != tc.want {
				t.Errorf("evalRules(%q) = %v, want %v", tc.cmd, got, tc.want)
			}
		})
	}
}

// TestEvalRules_DenylistWrapping proves that sh -c "sudoedit x" — whose inner
// dangerous segment is surfaced by splitSegments — still denies, driven
// end-to-end from the command string.
func TestEvalRules_DenylistWrapping(t *testing.T) {
	segs := segsFor(t, `sh -c "sudoedit x"`)
	got := evalRules(segs, AutoConfig{}, []string{"bash:*"}, nil, "")
	if got != VerdictDeny {
		t.Fatalf("evalRules(sh -c \"sudoedit x\") = %v, want VerdictDeny", got)
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
