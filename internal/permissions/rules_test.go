package permissions

import (
	"path/filepath"
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
)

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

// TestAllSegmentsReadOnlySafe_WriteTargetIsNotReadOnly pins the second half of
// change 0070 D4's safety argument. The safelist short-circuit (gate.go:660)
// runs with NO root context, so it cannot judge a redirect target: if it kept
// calling `echo x > /etc/passwd` read-only safe, the command would auto-approve
// there and never reach the heuristic layer that holds the roots.
//
// A segment carrying a write target is therefore never wholly read-only, and
// falls through to scoping — which allows the in-root case deterministically.
func TestAllSegmentsReadOnlySafe_WriteTargetIsNotReadOnly(t *testing.T) {
	cases := []struct {
		desc string
		cmd  string
		want bool
	}{
		{"read-only echo with a write target is not read-only", "echo x > /etc/passwd", false},
		{"an in-root target is equally not read-only here (no roots to check it against)", "ls > out.txt", false},
		{"one redirected segment sinks the whole command", "ls && wc -l x > out.txt", false},
		{"a compound statement's target sinks every segment", "{ ls; wc -l x; } > out.txt", false},
		// The 0037 benign shapes record no target and keep short-circuiting.
		{"a /dev/null sink stays read-only safe", "wc -l x 2>/dev/null", true},
		{"an fd-dup stays read-only safe", "ls 2>&1", true},
		{"an input redirect stays read-only safe", "cat < config.yaml", true},
		{"a here-doc stays read-only safe", "cat <<EOF\nhi\nEOF", true},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			if got := allSegmentsReadOnlySafe(segsFor(t, tc.cmd)); got != tc.want {
				t.Errorf("allSegmentsReadOnlySafe(%q) = %v, want %v", tc.cmd, got, tc.want)
			}
		})
	}
}

// TestEvalRules_ConfigAllowDoesNotLaunderARedirect closes the second no-root
// laundering path opened by change 0070 D4's widening. The rules layer runs
// BEFORE the safelist and, like it, has no root set: a config auto_approve
// pattern matches the reconstructed argv ("bash:echo x"), which does not and
// cannot mention the redirect. Consenting to `echo x` is not consenting to
// `echo x > /etc/passwd`.
//
// A redirected command therefore declines the rules-layer allow and falls
// through to path scoping, which allows the in-root case anyway — so a user's
// pattern still buys them `make > build.log` inside the workspace, just not a
// write to anywhere on the disk. Deny and ask precedence are untouched.
func TestEvalRules_ConfigAllowDoesNotLaunderARedirect(t *testing.T) {
	cases := []struct {
		desc        string
		cmd         string
		autoApprove []string
		want        Verdict
	}{
		{"an allowed command with a write target is not allowed here", "echo x > /etc/passwd", []string{"bash:*"}, VerdictAsk},
		{"an in-root target is equally deferred (no roots to check it against)", "make > build.log", []string{"bash:make*"}, VerdictAsk},
		{"the same command without a redirect still allows", "echo x", []string{"bash:*"}, VerdictAllow},
		{"a benign /dev/null sink records no target and still allows", "make > /dev/null", []string{"bash:make*"}, VerdictAllow},
		{"an input redirect records no target and still allows", "make < in.txt", []string{"bash:make*"}, VerdictAllow},
		// Deny still beats everything, redirect or not.
		{"deny still wins over the allow pattern", "rm -rf / > out.txt", []string{"bash:*"}, VerdictDeny},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			got := evalRules(segsFor(t, tc.cmd), config.AutoConfig{}, tc.autoApprove, nil, t.TempDir())
			if got != tc.want {
				t.Errorf("evalRules(%q) = %v, want %v", tc.cmd, got, tc.want)
			}
		})
	}
}

// TestIsReadOnlySafe_OpaqueArgs draws change 0070 D5's line through the
// read-only safe list.
//
// A readOnlyUtils name is read-only with ANY arguments, so an opaque one
// changes nothing: `cat $F` reads unknown data, and reading is not a mutation.
//
// The FLAG-INSPECTING names are the opposite case. isSafeSed, isSafeFind and
// isSafeGit decide by reading the argument words — `-i`, `-exec`, `push`. An
// opaque word could BE one of those, so the inspection is no longer a proof and
// the segment fails toward unsafe.
func TestIsReadOnlySafe_OpaqueArgs(t *testing.T) {
	cases := []struct {
		desc string
		cmd  string
		want bool
	}{
		{"cat of an opaque file is read-only", "cat $F", true},
		{"grep with an opaque pattern is read-only", "grep $PAT file", true},
		{"wc of an opaque file is read-only", "wc -l $f", true},
		{"echo of a substitution is read-only", "echo $(git rev-parse --show-toplevel)", true},
		{"sed with an opaque word could carry -i", "sed -n $F", false},
		{"sed with an opaque script operand", "sed $F x", false},
		{"find with an opaque word could carry -exec", "find . -name $PAT", false},
		{"git with an opaque word could carry push", "git $SUB", false},
		{"git log with an opaque revision", "git log $REV", false},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			segs := segsFor(t, tc.cmd)
			last := segs[len(segs)-1]
			if got := isReadOnlySafe(last); got != tc.want {
				t.Errorf("isReadOnlySafe(%q) = %v, want %v", tc.cmd, got, tc.want)
			}
		})
	}
}

// TestIsProvablyBenignKill_OpaqueOperand: the kill allow rests on every operand
// being a provable numeric PID. An opaque operand is not a number we read — it
// is a token we could not resolve — so the proof fails.
// `kill -$SIG 4242` is the row that makes this test load-bearing rather than
// decorative. Without the opaque guard the loop reads "-$SIG" as a signal FLAG
// (it begins with '-', so the flag arm swallows it), sees the literal 4242 as a
// provable PID, and returns TRUE — auto-approving `kill -9 4242`,
// `kill -SIGSTOP 4242`, or whatever $SIG expands to, with no human in the loop.
// The other rows would pass on an accident of the text: strconv.Atoi("$PID")
// happens to fail, so they never exercise the guard at all.
func TestIsProvablyBenignKill_OpaqueOperand(t *testing.T) {
	for _, cmd := range []string{"kill $PID", "kill -9 $PID", "kill 4242 $PID", "kill -$SIG 4242"} {
		segs := segsFor(t, cmd)
		if isProvablyBenignKill(segs[0]) {
			t.Errorf("isProvablyBenignKill(%q) = true — an opaque operand is not a provable PID", cmd)
		}
		if got := classifyHeuristic(segs, []string{t.TempDir()}); got != VerdictAsk {
			t.Errorf("classifyHeuristic(%q) = %v, want VerdictAsk", cmd, got)
		}
	}
}

// TestIsCatastrophicRm_OpaqueOperandIsNotResolved pins the invariant inside the
// DENY path too: isCatastrophicRm calls resolveExisting, which is exactly the
// walk-up-to-the-deepest-existing-ancestor the invariant forbids over an opaque
// operand. The verdict for `rm -rf $VAR` is the heuristic layer's ask, not a
// containment proof made over a token that is not a path.
func TestIsCatastrophicRm_OpaqueOperandIsNotResolved(t *testing.T) {
	root := t.TempDir()
	canon, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	restore := chdir(t, canon)
	defer restore()
	// `rm -rf $X/../..` is the row with teeth. filepath.Abs CLEANS the literal
	// token into a real path — <cwd>/$X/../.. collapses to the workspace's
	// GRANDPARENT — which resolveExisting then resolves happily, and the
	// workspace-ancestor test fires: without the skip this hard-DENIES. The
	// answer is bogus in both directions, because it is an answer about a path
	// built from a word that is not one: if $X were "a/b" the real target is the
	// workspace itself, and if $X were "/etc" it is "/". It also does NOT fire
	// on `rm -rf $X` where $X is literally "/", which is the genuinely
	// catastrophic case — so it is noise, not a security control.
	//
	// Removing it costs nothing in safety: every opaque rm still fails toward
	// the human at classifyHeuristic (asserted below), so nothing here can
	// auto-approve. What it buys is the invariant, literally — resolveExisting
	// is never handed a word that is not a path.
	for _, cmd := range []string{"rm -rf $VAR", "rm -rf $(echo /)", "rm -rf $X/../.."} {
		segs := segsFor(t, cmd)
		if isCatastrophicRm(segs[len(segs)-1], canon) {
			t.Errorf("isCatastrophicRm(%q) = true — an opaque operand must not be path-resolved", cmd)
		}
		if got := classifyHeuristic(segs, []string{canon}); got == VerdictAllow {
			t.Errorf("classifyHeuristic(%q) = VerdictAllow, want not-allow", cmd)
		}
	}
}

// TestEvalRules_RedirectTargetsReachTighteningPatterns covers the review's
// minor 6 for change 0070: a human who writes a deny (or always_prompt) pattern
// against a path expects it to cover the redirect FORM of writing to that path.
// Before the subject carried the targets, `echo secrets > .git/hooks/pre-commit`
// produced the subject "bash:echo secrets" and no path-shaped pattern could
// ever reach it.
//
// The second half of the table is the fail-open guard: widening the subject
// text must never make an auto_approve pattern match something it did not match
// before. It cannot, because allSegmentsAllowed declines any segment carrying a
// write target BEFORE the pattern is consulted — these rows pin that.
func TestEvalRules_RedirectTargetsReachTighteningPatterns(t *testing.T) {
	cases := []struct {
		desc         string
		cmd          string
		auto         config.AutoConfig
		autoApprove  []string
		alwaysPrompt []string
		want         Verdict
	}{
		{
			desc: "a path-shaped deny reaches the redirect target",
			cmd:  "echo secrets > .git/hooks/pre-commit",
			auto: config.AutoConfig{Deny: []string{"bash:* >*.git/hooks/*"}},
			want: VerdictDeny,
		},
		{
			// Shape pin, not a discriminator: a redirect-bearing segment can never
			// reach allow anyway, so this row is green either way. It records that
			// always_prompt sees the same widened subject the deny rows prove.
			desc:         "a path-shaped always_prompt reaches the redirect target",
			cmd:          "echo secrets > .git/hooks/pre-commit",
			autoApprove:  []string{"bash:*"},
			alwaysPrompt: []string{"bash:* >*.git/hooks/*"},
			want:         VerdictAsk,
		},
		{
			desc: "an append redirect is reachable too",
			cmd:  "echo secrets >> .git/hooks/pre-commit",
			auto: config.AutoConfig{Deny: []string{"bash:* >*.git/hooks/*"}},
			want: VerdictDeny,
		},
		{
			desc: "a deny naming only the argv still matches a redirected segment",
			cmd:  "echo secrets > out.txt",
			auto: config.AutoConfig{Deny: []string{"bash:echo secrets*"}},
			want: VerdictDeny,
		},
		{
			desc: "an unrelated path deny does not match",
			cmd:  "echo hi > out.txt",
			auto: config.AutoConfig{Deny: []string{"bash:* >*.git/hooks/*"}},
			want: VerdictAsk,
		},
		// Fail-open guard: the allow consumer's match set is unchanged.
		{
			desc:        "a broad allow still does not approve a redirect-bearing command",
			cmd:         "echo x > out.txt",
			autoApprove: []string{"bash:*"},
			want:        VerdictAsk,
		},
		{
			desc:        "an allow pattern spelled with the target form does not approve either",
			cmd:         "echo x > out.txt",
			autoApprove: []string{"bash:echo x >out.txt"},
			want:        VerdictAsk,
		},
		{
			desc:        "an unredirected command's allow is untouched",
			cmd:         "echo x",
			autoApprove: []string{"bash:echo *"},
			want:        VerdictAllow,
		},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			got := evalRules(segsFor(t, tc.cmd), tc.auto, tc.autoApprove, tc.alwaysPrompt, t.TempDir())
			if got != tc.want {
				t.Errorf("evalRules(%q) = %v, want %v", tc.cmd, got, tc.want)
			}
		})
	}
}
