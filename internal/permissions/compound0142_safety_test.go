package permissions

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
)

// gateInRoot builds an auto-mode gate the way the bindings do: the workspace
// root IS the process cwd. Without the Chdir, relative arguments resolve against
// the test package directory, which is outside a t.TempDir root, so routine
// commands look out-of-root and a test can pass for the wrong reason.
func gateInRoot(t *testing.T) (string, func(string) (Decision, bool)) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	return root, func(cmd string) (Decision, bool) {
		approve, called := newApproveRecorder(false)
		g := New(autoCfg(config.AutoConfig{}, nil, nil), newTestRegistry("bash"), Human(approve), WithWorkspaceRoot(root))
		var first Decision
		seen := false
		ctx := WithDecisionSink(context.Background(), func(d Decision) {
			if !seen {
				first, seen = d, true
			}
		})
		g.Execute(ctx, "bash", bashArgs(cmd))
		if !seen {
			t.Fatalf("%q: gate emitted no decision", cmd)
		}
		return first, *called
	}
}

// TestCdUnknownTarget_NeverLaundersLaterSegments is the regression pin for the
// hole in change 0142's cd widening. Path args are scoped against the original
// root, but the shell resolves them against wherever the cd landed, so once the
// target is not statically known, a relative path proves nothing. None of these
// may auto-approve.
func TestCdUnknownTarget_NeverLaundersLaterSegments(t *testing.T) {
	_, decide := gateInRoot(t)
	for _, cmd := range []string{
		// Literal-looking substitutions and variables.
		`cd "$(echo /outside)" && rm -f victim`,
		`cd "$(echo /outside)" && rm -rf .`,
		`cd "$(echo /outside)" && rm -rf *`,
		`cd "$(echo /outside)" && mv victim v2`,
		`cd "$(echo /outside)" && tee victim`,
		`cd "$(echo /outside)" && echo pwned > victim`,
		`cd "$HOME" && rm -f victim`,
		`cd "$HOME" && echo x >> .zshrc`,
		`cd "$HOME/.ssh" && rm -f id_ed25519`,
		`cd "$HOME/.ssh" && touch authorized_keys`,
		`cd "$HOME/.ssh" && cat id_ed25519`,
		// Near-misses of the benign $(pwd) shape: an exact-match exemption must
		// not let these through.
		`cd "$(pwd)/.." && rm -f victim`,
		`cd "$PWD/.." && rm -f victim`,
		`cd "$(cd /; pwd)" && rm -f victim`,
		`cd "$(pwd)" && cd "$HOME" && rm -f victim`,
		`cd -P "$HOME" && rm -f victim`,
		// Every list operator, not only &&.
		`cd "$HOME"; rm -f victim`,
		`cd "$HOME" || rm -f victim`,
		// cd forms with no opaque word whose target is still not the root.
		`cd && rm -f victim`,
		`cd - && rm -f victim`,
		`HOME=/etc cd && rm -f passwd`,
		`CDPATH=/etc cd ssh && rm -f sshd_config`,
	} {
		t.Run(cmd, func(t *testing.T) {
			if d, _ := decide(cmd); d.Verdict == "allow" {
				t.Errorf("auto-approved at layer %q: the cd target is unknown, so relative paths after it are unproven", d.Layer)
			}
		})
	}
}

// TestCdCurrentDir_StaysAutoApproved pins what the widening exists to deliver,
// so fixing the hole above cannot quietly remove it: a cd to the directory the
// shell is already in moves nothing, and the segments after it stand on their own.
func TestCdCurrentDir_StaysAutoApproved(t *testing.T) {
	_, decide := gateInRoot(t)
	for _, cmd := range []string{
		`cd "$(pwd)" && git status`,
		`cd "$(pwd)" && git diff`,
		`cd $(pwd) && git status`,
		`cd "$PWD" && git status`,
		`cd "$(pwd)" && make test`,
		`cd "$(pwd)" && rm -f in-root.txt`,
	} {
		t.Run(cmd, func(t *testing.T) {
			if d, called := decide(cmd); d.Verdict != "allow" || called {
				t.Errorf("verdict = %q at layer %q (asked human: %v), want a deterministic allow", d.Verdict, d.Layer, called)
			}
		})
	}
}

// TestCdCurrentDir_KeepsLaterSegmentsScoped proves the exemption covers only the
// cd: an out-of-root or catastrophic segment after a benign cd still stops.
func TestCdCurrentDir_KeepsLaterSegmentsScoped(t *testing.T) {
	_, decide := gateInRoot(t)
	for _, tc := range []struct{ cmd, want string }{
		{`cd "$(pwd)" && rm -f /etc/hosts`, "ask"},
		{`cd "$(pwd)" && echo x > /etc/hosts`, "ask"},
		{`cd "$(pwd)" && curl https://example.com`, "ask"},
		{`cd "$(pwd)" && rm -rf ~`, "deny"},
	} {
		t.Run(tc.cmd, func(t *testing.T) {
			if d, _ := decide(tc.cmd); d.Verdict != tc.want {
				t.Errorf("verdict = %q at layer %q, want %q", d.Verdict, d.Layer, tc.want)
			}
		})
	}
}

// TestComposition_NotAutoApproved covers design item 2 of the spec, "composition
// is not free": a shell fed by a pipe and an executed substitution stay off the
// auto-approve path even when every producer is individually read-only. The
// change's own test covered only curl, which is stopped by egress regardless.
func TestComposition_NotAutoApproved(t *testing.T) {
	_, decide := gateInRoot(t)
	for _, cmd := range []string{
		`cat script | sh`,
		`echo "rm -rf ~" | bash`,
		`git log | sh`,
		`cd "$(pwd)" && cat script | sh`,
		`$(cat script)`,
		`eval "$(cat script)"`,
		`sh -c "$(cat script)"`,
		`bash -c "$(cat script)"`,
		`cat list | xargs rm -rf`,
		`find . -name x -exec rm -rf {} ;`,
	} {
		t.Run(cmd, func(t *testing.T) {
			if d, _ := decide(cmd); d.Verdict == "allow" {
				t.Errorf("auto-approved at layer %q: the composition executes data", d.Layer)
			}
		})
	}
}

// TestParseLayer_RoutineCompoundsDecompose pins the outcome change 0142 set out
// to deliver. These shapes are reduced from real approval history, where each
// was refused by the parser and prompted a human: `LayerParse` must stop being
// the catch-all bucket for routine compounds (spec, design item 4). The verdict
// is deliberately not asserted; the classifier may still judge a shape gray.
func TestParseLayer_RoutineCompoundsDecompose(t *testing.T) {
	_, decide := gateInRoot(t)
	for _, cmd := range []string{
		`cd "$(pwd)" && docket change attach-plan --id 129 --markdown plan.md --revision "$(git rev-parse HEAD)"`,
		`cd "$(pwd)" && REV="abc123" && echo "$REV" && docket status`,
		`cd "$(pwd)" && HEAD_SHA=$(git rev-parse HEAD) && echo "$HEAD_SHA"`,
		"cd \"$(pwd)\" && cat <<'EOF' | docket adr record --request - --json\n{\"title\":\"x\"}\nEOF",
		`cd "$(pwd)" && git status 2>&1 | tail -30; echo "===EXIT: ${PIPESTATUS[0]}==="`,
		`for f in a.txt b.txt; do cat "$f"; done`,
		`cd "$(pwd)" && docket change attach-plan --id 129 --revision "$(git -C '.docket' rev-parse HEAD)"`,
		// Reaches the classifier, not auto-approve: an opaque word could expand to git log --output=x.
		`git log --oneline -1 "$(git rev-parse HEAD)"`,
		"cd \"$(pwd)\" && cat <<EOF | docket change attach-result --request - --repo-dir \"$(pwd)\" --json 2>&1 | tail -5\n{\"sha\":\"abc\"}\nEOF",
	} {
		t.Run(cmd, func(t *testing.T) {
			if d, _ := decide(cmd); d.Layer == LayerParse {
				t.Errorf("refused at the parse layer; a routine compound must reach per-segment evaluation")
			}
		})
	}
}

// TestParseLayer_ExecutedSubstitutionStillFailsClosed pins the invariant the
// parser work above must not trade away.
func TestParseLayer_ExecutedSubstitutionStillFailsClosed(t *testing.T) {
	_, decide := gateInRoot(t)
	for _, cmd := range []string{
		`cd "$(rm -rf ~)" && ls`,
		`echo $(rm -rf /)`,
		`cat "$(touch /tmp/x)"`,
	} {
		t.Run(cmd, func(t *testing.T) {
			if d, _ := decide(cmd); d.Verdict == "allow" {
				t.Errorf("auto-approved at layer %q", d.Layer)
			}
		})
	}
}

// TestPromptAllMode_CompoundStillPrompts pins "other modes are behavior
// identical": the auto-mode widening never reaches prompt-all.
func TestPromptAllMode_CompoundStillPrompts(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	approve, called := newApproveRecorder(true)
	cfg := config.PermissionsConfig{Mode: "prompt-all"}
	g := New(cfg, newTestRegistry("bash"), Human(approve), WithWorkspaceRoot(root))
	g.Execute(context.Background(), "bash", bashArgs(`cd "$(pwd)" && git status`))
	if !*called {
		t.Error("prompt-all must prompt for every bash call")
	}
}

// TestShellStateChange_NotAutoApproved pins what the $(pwd) exemption relies on:
// that pwd, cd and the commands after them mean what they usually mean. An alias
// on an earlier line is honored by /bin/sh, so these really do run elsewhere, and
// assignments that steer resolution are unsafe however the parser comes to accept
// them. The assignments are refused today only because the parser fails closed.
func TestShellStateChange_NotAutoApproved(t *testing.T) {
	_, decide := gateInRoot(t)
	for _, cmd := range []string{
		"alias pwd='echo /outside'\ncd \"$(pwd)\" && rm -f victim",
		"alias pwd='echo /outside'; cd \"$(pwd)\" && rm -f victim",
		"alias cd='cd /outside'\ncd \"$(pwd)\" && rm -f victim",
		"alias rm='rm -rf /outside'\nrm x",
		"alias ls='rm -rf /outside'\nls",
		"unalias -a; cd \"$(pwd)\" && rm -f victim",
		"shopt -s expand_aliases\nalias rm='rm -rf /outside'\nrm x",
		"pwd() { echo /outside; }\ncd \"$(pwd)\" && rm -f victim",
		"cd() { builtin cd /outside; }\ncd \"$(pwd)\" && rm -f victim",
		"PWD=/outside; cd \"$PWD\" && rm -f victim",
		"PWD=/outside\ncd \"$PWD\" && rm -f victim",
		"export PWD=/outside && cd \"$PWD\" && rm -f victim",
		"OLDPWD=/outside; cd - && rm -f victim",
		"HOME=/outside; cd && rm -f victim",
		"CDPATH=/outside; cd sub && rm -f victim",
		"export CDPATH=/outside && cd sub && rm -f victim",
		"PATH=/outside:$PATH; make test",
		"export PATH=/outside && make test",
		"IFS=/; make test",
		"LD_PRELOAD=/outside/x.so; make test",
		"export BASH_ENV=/outside/x && bash -c ls",
		"ENV=/outside/x; sh -c ls",
		"FOO=/outside; cd \"$FOO\" && rm -f victim",
		"FOO=$(echo /outside); cd \"$FOO\" && rm -f victim",
		"FOO=/outside; rm -f \"$FOO/victim\"",
	} {
		t.Run(cmd, func(t *testing.T) {
			if d, _ := decide(cmd); d.Verdict == "allow" {
				t.Errorf("auto-approved at layer %q: the command changes what later segments mean", d.Layer)
			}
		})
	}
}
