package permissions

import (
	"path"
	"strings"
	"testing"
)

// TestMatchProtective pins the generous glob protective consumers use: '*'
// and '?' cross '/', while classes, escapes and malformed-pattern handling
// follow path.Match exactly (change 0088).
func TestMatchProtective(t *testing.T) {
	cases := []struct {
		pat, subject string
		want         bool
	}{
		// The fail-open this change closes: '*' must span '/'.
		{"bash:rm *", "bash:rm -rf a/b", true},
		{"bash:cat*", "bash:cat dir/file", true},
		{"a*b*c", "a/x/b/y/c", true},
		{"a*b*c", "a/x/b/y/d", false},
		// '?' matches exactly one character, including '/'.
		{"bash:rm -rf a?b", "bash:rm -rf a/b", true},
		{"bash:rm -rf a?b", "bash:rm -rf a//b", false},
		// Classes follow path.Match rules.
		{"bash:rm -[rR]f *", "bash:rm -Rf a/b", true},
		{"bash:rm -[^rR]f *", "bash:rm -Rf a/b", false},
		{"bash:[^a]*", "bash:/x", true},
		// Escapes match the literal character.
		{`bash:\*`, "bash:*", true},
		{`bash:\*`, "bash:x", false},
		// Anchored at both ends, like path.Match.
		{"bash:rm", "bash:rm", true},
		{"bash:rm *", "bash:rmdir x", false},
		{"bash:rm", "bash:rm -rf a", false},
		{"", "", true},
		{"", "x", false},
		{"*", "", true},
		// Redirect-target subjects (change 0070 review minor 6) keep matching.
		{"bash:* >*.git/hooks/*", "bash:echo secrets >.git/hooks/pre-commit", true},
		{"bash:* >*.git/hooks/*", "bash:echo hi >out.txt", false},
		// A malformed pattern matches nothing, exactly as path.Match's ignored
		// ErrBadPattern did before.
		{"bash:rm [", "bash:rm [", false},
		{"[]a]", "]", false},
		{`bash:rm \`, `bash:rm \`, false},
	}
	for _, tc := range cases {
		if got := matchProtective(tc.pat, tc.subject); got != tc.want {
			t.Errorf("matchProtective(%q, %q) = %v, want %v", tc.pat, tc.subject, got, tc.want)
		}
	}
}

// FuzzMatchProtective proves the two properties the design rests on:
//
//  1. Superset: whenever path.Match matches, matchProtective matches, so no
//     protective rule that fired before this change stops firing.
//  2. Equivalence off the slash: for a subject with no '/', the two engines
//     agree exactly, so the only behavior change is '*' and '?' crossing '/'.
//
// A malformed pattern (path.Match error) must match nothing.
func FuzzMatchProtective(f *testing.F) {
	for _, p := range []string{"bash:rm *", "a*b", "[a-c]?", `\*x`, "[^/]*", "*a*a*", "bash:git push*"} {
		f.Add(p, "bash:rm -rf a/b")
		f.Add(p, "abc")
	}
	f.Fuzz(func(t *testing.T, pat, subject string) {
		want, err := path.Match(pat, subject)
		got := matchProtective(pat, subject)
		if err != nil {
			if got {
				t.Fatalf("malformed pattern %q matched %q", pat, subject)
			}
			return
		}
		if want && !got {
			t.Fatalf("superset violated: path.Match(%q, %q) matched, matchProtective did not", pat, subject)
		}
		if !strings.Contains(subject, "/") && got != want {
			t.Fatalf("no-slash disagreement for (%q, %q): path.Match=%v matchProtective=%v", pat, subject, want, got)
		}
	})
}
