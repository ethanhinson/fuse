<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0088 — Protective permission patterns match across slashes and in every mode alike](../../changes/active/0088-protective-permission-patterns-match-across-slashes-and-in-e.md)**
<!-- docket:backlink:end -->

# Protective Permission Patterns Match Across Slashes: Implementation Plan

> **For agentic workers:** this plan is executed by `docket-build`, task by task, in order. Each task is one focused test-first unit ending in exactly one commit. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Protective permission patterns (`auto.deny`, `auto.ask`, `always_prompt`) match with a glob whose `*` spans `/`, against one subject set shared by smart and auto mode, while allow patterns (`auto_approve`) keep strict `path.Match` so no existing allow widens.

**Architecture:** A new `matchProtective` glob in `internal/permissions/cmdglob.go` mirrors `path.Match` byte for byte except that `*` and `?` may consume `/`. `matchesSubjects` and `matchesSegment` in `rules.go` take a consumer kind (protective or permissive). One shared function, `protectiveHit(patterns, segments)`, matches protective patterns per parsed segment (`bash:<argv0> <args>` and the head `bash:<argv0>`) and against the whole command. `evalRules` (auto) and the smart-mode branch in `gate.go` both call it, so the two modes cannot drift. Smart mode keeps its legacy `{tool, bash:<firstToken>}` subjects for both consumers (nothing that matched before stops matching) and gains the per-segment subject: for allow it reuses `allSegmentsAllowed` (every segment must match strictly, with the existing opaque-arg and write-target guards), for protective it uses `protectiveHit`.

**Tech Stack:** Go 1.26, standard `testing` package (table tests plus one native fuzz target). No new dependencies.

**Change:** 0088. **Spec:** `docs/superpowers/specs/2026-10-31-protective-permission-patterns-match-across-slashes-and-in-e-design.md`. **Related ADRs:** ADR-0005 (per-segment allow evaluation), ADR-0049 (allowlist admission on deterministic allow paths). The parent records the new ADR (protective patterns match generously, permissive patterns match strictly); this plan has no ADR task.

---

## Ground rules for every task

- Keep the diff inside `internal/permissions/` plus `README.md`. PR #110 (change 0087) also edits `rules.go` and `gate.go` and will be rebased against this branch later: touch only the exact spots named below, do not reflow or reword unrelated comments, do not rename existing functions other than the ones this plan names.
- Do not use em dashes in code, comments, test names, commit messages, or docs. Use a colon, comma, parentheses, or a new sentence instead.
- Do not change default rule sets, do not add regex or any new pattern syntax, and do not touch the explicit-ask verdict or headless policy (separate change 0087).
- Fetch-host globs (`fetchhost.go`, `matchesAnyGlob`) are out of scope: hosts carry no `/`.
- Non-bash tools in auto mode do not consult `always_prompt` today (`resolveAuto` allows any `onSafeList` tool before rules). That is a separate gap; do not fix it here.
- Every task ends with `gofmt -l internal/permissions` printing nothing, `go vet ./internal/permissions/` clean, and `go test ./internal/permissions/...` green.

## Known behavior on this base (do not "fix" it)

On the current base an auto-mode `VerdictAsk` from `evalRules` is not terminal: `resolveAuto` continues to the read-only safelist, which can still allow (for example `cat dir/file`). Change 0087 makes explicit ask terminal. Therefore:

- The `bash:cat*` ask case is asserted at the `evalRules` layer (rules verdict), not end to end in auto.
- End-to-end parity rows use only commands that are NOT on the read-only safelist (`rm`, `mv`, `git commit`, `npm publish`), so downstream layers cannot mask a protective hit.

## File map

| File | Action | Responsibility |
|---|---|---|
| `internal/permissions/cmdglob.go` | Create | `matchProtective(pat, subject)`: generous glob (`*` and `?` span `/`), malformed pattern matches nothing |
| `internal/permissions/cmdglob_test.go` | Create | Table test plus `FuzzMatchProtective` (superset of `path.Match`; identical when the subject has no `/`) |
| `internal/permissions/rules.go` | Modify | `patternConsumer` kind; `matchesSubjects(kind, ...)`; `matchesSegment(kind, ...)` adds head subject for protective; new `protectiveHit`; `evalRules` deny/ask use `protectiveHit`; allow uses permissive |
| `internal/permissions/rules_test.go` | Modify | Protective-crosses-slash table; permissive-stays-strict table; permissive engine pin against `path.Match` |
| `internal/permissions/patterns.go` | Modify | Replace `matchesAny` with `matchesProtective` and `matchesPermissive` |
| `internal/permissions/patterns_test.go` | Create | Unit tables for the two smart-mode matchers |
| `internal/permissions/gate.go` | Modify | Smart branch (around lines 563-565) calls the two new matchers |
| `internal/permissions/gate_test.go` | Modify | Smart end-to-end tests: slash-bearing always_prompt demotes; strict allow still misses |
| `internal/permissions/patternparity_test.go` | Create | Same protective table in smart and auto, matcher level and end to end, identical verdicts |
| `README.md` | Modify | One "How patterns match" paragraph in the permissions section |

---

### Task 1: Generous command glob `matchProtective`

**Suggested tier:** standard

**Files:**
- Create: `internal/permissions/cmdglob.go`
- Test: `internal/permissions/cmdglob_test.go`

- [ ] **Step 1: Write the failing tests**

Create `internal/permissions/cmdglob_test.go`:

```go
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
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/permissions/ -run 'TestMatchProtective|FuzzMatchProtective' -count=1`
Expected: FAIL to compile with `undefined: matchProtective`.

- [ ] **Step 3: Implement `cmdglob.go`**

Create `internal/permissions/cmdglob.go`. This implementation was prototyped and fuzzed (56M executions, no counterexample) against both properties above; keep its byte-level comparisons, they are what makes it agree with `path.Match` on invalid UTF-8.

```go
package permissions

import (
	"path"
	"unicode/utf8"
)

// matchProtective reports whether subject matches pat under the generous glob
// that PROTECTIVE consumers (auto.deny, auto.ask, always_prompt) use (change
// 0088). It is path.Match with one difference: '*' matches any run of
// characters INCLUDING '/', and '?' matches any one character including '/'.
// Character classes ('[...]', '[^...]', ranges) and '\' escapes follow
// path.Match exactly.
//
// Why two engines: path.Match's '*' stops at '/', which is the containment an
// ALLOW pattern like "bash:rm build/*" relies on (ADR-0005, ADR-0049), so the
// permissive consumer keeps it. For a deny or ask rule that same boundary is a
// fail-open: "bash:rm *" missed "rm -rf a/b". A protective match only ever
// tightens, so matching more text is the safe direction.
//
// A malformed pattern matches nothing, which is how the old
// `ok, _ := path.Match(...)` call sites already treated ErrBadPattern.
func matchProtective(pat, subject string) bool {
	// path.Match validates the whole pattern even when it does not match, so
	// probing against "" is a complete syntax check.
	if _, err := path.Match(pat, ""); err != nil {
		return false
	}
	return globSpanSlash(pat, subject)
}

// globSpanSlash matches a pre-validated pattern. It is the classic
// single-star-backtrack wildcard loop: on a mismatch it resumes at the most
// recent '*', letting it swallow one more byte. Literal and escaped bytes are
// compared byte for byte and '*' advances one byte at a time, mirroring
// path.Match; '?' and classes consume one rune, also mirroring path.Match.
func globSpanSlash(pat, s string) bool {
	px, sx := 0, 0
	starPx, starSx := -1, 0
	for px < len(pat) || sx < len(s) {
		if px < len(pat) {
			switch pat[px] {
			case '*':
				starPx, starSx = px, sx
				px++
				continue
			case '?':
				if sx < len(s) {
					_, n := utf8.DecodeRuneInString(s[sx:])
					px++
					sx += n
					continue
				}
			case '[':
				if sx < len(s) {
					r, n := utf8.DecodeRuneInString(s[sx:])
					if ok, end := matchGlobClass(pat, px, r); ok {
						px = end
						sx += n
						continue
					}
				}
			case '\\':
				// Validation guarantees an escape is followed by a byte.
				if sx < len(s) && pat[px+1] == s[sx] {
					px += 2
					sx++
					continue
				}
			default:
				if sx < len(s) && pat[px] == s[sx] {
					px++
					sx++
					continue
				}
			}
		}
		if starPx >= 0 && starSx < len(s) {
			starSx++
			px, sx = starPx+1, starSx
			continue
		}
		return false
	}
	return true
}

// matchGlobClass reports whether r matches the character class that starts at
// pat[px] == '[', and returns the index just past its closing ']'. The pattern
// is pre-validated, so the class is well formed and non-empty.
func matchGlobClass(pat string, px int, r rune) (bool, int) {
	i := px + 1
	negated := false
	if i < len(pat) && pat[i] == '^' {
		negated = true
		i++
	}
	matched := false
	for n := 0; ; n++ {
		if n > 0 && pat[i] == ']' {
			i++
			break
		}
		lo, w := globClassChar(pat, i)
		i += w
		hi := lo
		if pat[i] == '-' {
			h, w2 := globClassChar(pat, i+1)
			hi = h
			i += 1 + w2
		}
		if lo <= r && r <= hi {
			matched = true
		}
	}
	return matched != negated, i
}

// globClassChar decodes one (possibly '\'-escaped) class character at pat[i]
// and returns it with its encoded width.
func globClassChar(pat string, i int) (rune, int) {
	if pat[i] == '\\' {
		r, n := utf8.DecodeRuneInString(pat[i+1:])
		return r, 1 + n
	}
	return utf8.DecodeRuneInString(pat[i:])
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/permissions/ -run 'TestMatchProtective|FuzzMatchProtective' -count=1 -v`
Expected: PASS (the fuzz target runs its seed corpus only).

Then run the fuzzer briefly: `go test ./internal/permissions/ -run '^$' -fuzz FuzzMatchProtective -fuzztime 30s`
Expected: PASS with no new failing input. If it finds one, fix the matcher (not the property), and keep the generated `testdata/fuzz/FuzzMatchProtective/<hash>` file in the commit as a regression seed.

- [ ] **Step 5: Full package check and commit**

Run: `gofmt -l internal/permissions && go vet ./internal/permissions/ && go test ./internal/permissions/... -count=1`
Expected: no gofmt output, vet clean, all PASS.

```bash
git add internal/permissions/cmdglob.go internal/permissions/cmdglob_test.go
git commit -m "permissions: add a slash-spanning glob for protective patterns

matchProtective is path.Match with '*' and '?' allowed to cross '/'.
A fuzz target pins it as a strict superset of path.Match that agrees
exactly on slash-free subjects. Not wired in yet (change 0088)."
```

---

### Task 2: Consumer kind in the rules layer; deny and ask match generously

**Suggested tier:** premium (security-relevant matcher used by every auto-mode deny/ask decision; mistakes are correctable but would fail open or over-deny)

**Files:**
- Modify: `internal/permissions/rules.go` (`evalRules` around lines 81-117, `allSegmentsAllowed` line 154, `matchesSegment` around 571-576, `matchesSubjects` around 626-636)
- Test: `internal/permissions/rules_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `internal/permissions/rules_test.go` (it already imports `path/filepath`, `strings`, `testing`, `config`; add `"path"` to the import block for the pin test):

```go
// TestEvalRules_ProtectivePatternsCrossSlashes is change 0088's fail-open fix:
// deny, ask and always_prompt patterns match with a glob whose '*' spans '/',
// per segment, against the segment head, and against the whole command. Each
// ask row carries a witness allow that WOULD approve the command, so an ask
// verdict proves the protective pattern fired (the default is also ask).
func TestEvalRules_ProtectivePatternsCrossSlashes(t *testing.T) {
	cases := []struct {
		desc         string
		cmd          string
		auto         AutoConfig
		autoApprove  []string
		alwaysPrompt []string
		want         Verdict
	}{
		{desc: "deny bash:rm * denies an argument with a slash", cmd: "rm -rf a/b",
			auto: AutoConfig{Deny: []string{"bash:rm *"}}, want: VerdictDeny},
		{desc: "deny bash:rm * denies the slash segment of a compound", cmd: "rm -rf a/b && ls",
			auto: AutoConfig{Deny: []string{"bash:rm *"}}, want: VerdictDeny},
		{desc: "deny on the bare head bash:rm reaches every rm invocation", cmd: "rm -rf a/b",
			auto: AutoConfig{Deny: []string{"bash:rm"}}, want: VerdictDeny},
		{desc: "deny matches the head of a peeled wrapper", cmd: "timeout 5 rm -rf a/b",
			auto: AutoConfig{Deny: []string{"bash:rm"}}, want: VerdictDeny},
		{desc: "ask bash:cat* asks for cat dir/file over an allow", cmd: "cat dir/file",
			auto: AutoConfig{Ask: []string{"bash:cat*"}}, autoApprove: []string{"bash:cat dir/*"}, want: VerdictAsk},
		{desc: "always_prompt bash:rm * asks over an allow", cmd: "rm -rf a/b",
			autoApprove: []string{"bash:rm -rf a/*"}, alwaysPrompt: []string{"bash:rm *"}, want: VerdictAsk},
		{desc: "a whole-command protective pattern spans segment separators and slashes", cmd: "git status && rm -rf a/b",
			auto: AutoConfig{Deny: []string{"bash:git status*rm -rf a/*"}}, want: VerdictDeny},
		{desc: "an unrelated protective pattern still misses", cmd: "mv a/b c/d",
			auto: AutoConfig{Deny: []string{"bash:rm *"}}, autoApprove: []string{"bash:mv a/* c/*"}, want: VerdictAllow},
		{desc: "a malformed protective pattern matches nothing", cmd: "rm -rf a/b",
			auto: AutoConfig{Deny: []string{"bash:rm ["}}, autoApprove: []string{"bash:rm -rf a/*"}, want: VerdictAllow},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			got := evalRules(segsFor(t, tc.cmd), tc.auto, tc.autoApprove, tc.alwaysPrompt, "")
			if got != tc.want {
				t.Errorf("evalRules(%q) = %v, want %v", tc.cmd, got, tc.want)
			}
		})
	}
}

// TestEvalRules_PermissivePatternsStayStrict pins the other half of change
// 0088: auto_approve keeps path.Match, whose '*' does not cross '/', so the
// containment a human wrote into an allow pattern still holds (ADR-0005,
// ADR-0049). None of these may become VerdictAllow.
func TestEvalRules_PermissivePatternsStayStrict(t *testing.T) {
	cases := []struct {
		desc        string
		cmd         string
		autoApprove []string
	}{
		{"bash:git * still misses a URL with slashes", "git clone https://x/y", []string{"bash:git *"}},
		{"bash:git * does not launder a trailing rm (per segment)", "git status && rm -rf a/b", []string{"bash:git *"}},
		{"bash:rm build/* does not admit a parent escape", "rm build/../x", []string{"bash:rm build/*"}},
		{"bash:* does not cross a slash in an argument", "rm -rf a/b", []string{"bash:*"}},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			if got := evalRules(segsFor(t, tc.cmd), AutoConfig{}, tc.autoApprove, nil, ""); got == VerdictAllow {
				t.Errorf("evalRules(%q) with allow %v = allow, want not allow", tc.cmd, tc.autoApprove)
			}
		})
	}
}

// TestMatchesSubjects_PermissiveIsPathMatch pins the permissive engine to
// path.Match byte for byte, so every existing allow decision is unchanged.
// Each side gets its own production source: the old engine is path.Match
// itself, not a copy of it.
func TestMatchesSubjects_PermissiveIsPathMatch(t *testing.T) {
	pats := []string{"bash:git *", "bash:rm build/*", "bash:*", "bash:go", "read_file", "bash:echo x >out.txt", "bash:rm ["}
	subjects := []string{
		"bash:git status", "bash:git clone https://x/y", "bash:rm build/out", "bash:rm build/../x",
		"bash:rm -rf a/b", "bash:go", "read_file", "bash:echo x >out.txt", "bash:rm [",
	}
	for _, p := range pats {
		for _, s := range subjects {
			want, _ := path.Match(p, s)
			if got := matchesSubjects(consumerPermissive, []string{p}, s); got != want {
				t.Errorf("permissive matchesSubjects(%q, %q) = %v, path.Match = %v", p, s, got, want)
			}
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/permissions/ -run 'TestEvalRules_ProtectivePatternsCrossSlashes|TestEvalRules_PermissivePatternsStayStrict|TestMatchesSubjects_PermissiveIsPathMatch' -count=1`
Expected: FAIL to compile (`undefined: consumerPermissive`; `matchesSubjects` has the wrong arity).

- [ ] **Step 3: Implement the consumer kind in `rules.go`**

3a. Replace `matchesSubjects` (around lines 626-636) with:

```go
// patternConsumer says which way a pattern match points (change 0088).
// Protective consumers (auto.deny, auto.ask, always_prompt) only ever tighten,
// so they match generously with matchProtective, whose '*' spans '/'.
// The permissive consumer (auto_approve) turns a match into an authorization,
// so it keeps path.Match, whose '*' stops at '/': that boundary is the
// containment a human writing "bash:rm build/*" relies on (ADR-0005, ADR-0049).
type patternConsumer int

const (
	consumerProtective patternConsumer = iota
	consumerPermissive
)

// matchesSubjects matches patterns against a single subject with the engine
// for the given consumer: matchProtective for protective, path.Match for
// permissive. A malformed pattern matches nothing under either engine.
func matchesSubjects(kind patternConsumer, patterns []string, subject string) bool {
	for _, pat := range patterns {
		if kind == consumerProtective {
			if matchProtective(pat, subject) {
				return true
			}
			continue
		}
		if ok, _ := path.Match(pat, subject); ok {
			return true
		}
	}
	return false
}
```

3b. Replace `matchesSegment` and its doc comment (around lines 571-576) with:

```go
// matchesSegment matches patterns against a single segment's reconstructed
// bash subject line (e.g. "bash:git status"), per segment against the parsed
// argv rather than the raw source text. A protective consumer is also matched
// against the segment head "bash:<argv0>", so a rule written in the legacy
// first-word shape ("bash:git") means the same thing in every mode (change
// 0088). The permissive consumer never sees the head: a bare "bash:git" allow
// must not approve every git invocation in auto mode.
func matchesSegment(kind patternConsumer, patterns []string, seg Segment) bool {
	if matchesSubjects(kind, patterns, segmentSubject(seg)) {
		return true
	}
	return kind == consumerProtective && matchesSubjects(kind, patterns, "bash:"+seg.Name)
}

// protectiveHit reports whether any protective pattern matches the parsed
// command: per segment (full subject and head) or against the whole command.
// It is the ONE protective subject rule; auto mode (evalRules) and smart mode
// (matchesProtective) both call it, so the two modes cannot drift apart
// (change 0088).
func protectiveHit(patterns []string, segments []Segment) bool {
	if len(patterns) == 0 || len(segments) == 0 {
		return false
	}
	for _, seg := range segments {
		if matchesSegment(consumerProtective, patterns, seg) {
			return true
		}
	}
	return matchesSubjects(consumerProtective, patterns, wholeSubject(segments))
}
```

3c. In `evalRules` (around lines 81-106), delete the line `whole := wholeSubject(segments)` and its following blank line, then make the deny and ask blocks read:

```go
	// 1. Deny wins globally.
	for _, seg := range segments {
		if isDangerous(seg, workspaceRoot) {
			return VerdictDeny
		}
	}
	if protectiveHit(cfg.Deny, segments) {
		return VerdictDeny
	}

	// 2. Ask beats allow.
	askPatterns := append(append([]string(nil), cfg.Ask...), alwaysPrompt...)
	if protectiveHit(askPatterns, segments) {
		return VerdictAsk
	}
```

Leave steps 3 and 4 of `evalRules` exactly as they are. In the doc comment above `evalRules`, replace only the final paragraph (the one starting "Deny and ask patterns are matched against each segment's argv AND the whole") with:

```go
// Deny and ask patterns are protective: they match with matchProtective
// (whose '*' spans '/') against each segment's argv, each segment's head, and
// the whole command string (protectiveHit). Allow is permissive: per segment
// only, with path.Match, so a whole-string allow can never rescue a dangerous
// segment and an allow's '/' containment holds (change 0088).
```

3d. In `allSegmentsAllowed` (line 154), change `matchesSegment(autoApprove, seg)` to `matchesSegment(consumerPermissive, autoApprove, seg)`. Do not edit the long comment above it; it is still accurate (allow still uses `path.Match`).

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/permissions/ -run 'TestEvalRules|TestMatchesSubjects' -count=1 -v`
Expected: PASS, including every pre-existing `TestEvalRules_*` table (precedence, redirect targets, opaque args) unchanged.

- [ ] **Step 5: Full package check and commit**

`patterns.go` still compiles in this task because `matchesAny` calls `path.Match` directly and does not use `matchesSubjects`. Confirm:

Run: `gofmt -l internal/permissions && go vet ./internal/permissions/ && go test ./internal/permissions/... -count=1`
Expected: clean, all PASS.

```bash
git add internal/permissions/rules.go internal/permissions/rules_test.go
git commit -m "permissions: match deny and ask patterns across slashes in auto mode

Protective consumers (auto.deny, auto.ask, always_prompt) now use the
slash-spanning glob and also match each segment's bash:<argv0> head,
through one shared protectiveHit. auto_approve keeps strict path.Match,
pinned byte for byte, so no allow widens. Fixes bash:rm * missing
rm -rf a/b (change 0088)."
```

---

### Task 3: Smart mode uses the same subject rule

**Suggested tier:** premium (changes the smart-mode allow and always_prompt decisions for every bash call)

**Files:**
- Modify: `internal/permissions/patterns.go` (replace `matchesAny`, lines 9-22)
- Modify: `internal/permissions/gate.go` (smart branch, lines 563-565 only)
- Create: `internal/permissions/patterns_test.go`
- Modify: `internal/permissions/gate_test.go`

- [ ] **Step 1: Write the failing unit tests**

Create `internal/permissions/patterns_test.go` (`bashArgs` already exists in `auto_test.go` in the same package):

```go
package permissions

import "testing"

// TestMatchesProtective covers smart mode's always_prompt matcher (change
// 0088): legacy {tool, bash:<firstToken>} subjects are kept, and the parsed
// per-segment, head and whole-command subjects are added with the
// slash-spanning glob.
func TestMatchesProtective(t *testing.T) {
	cases := []struct {
		desc     string
		patterns []string
		tool     string
		cmd      string // bash command; ignored for non-bash tools
		want     bool
	}{
		{"slash argument now matches", []string{"bash:rm *"}, "bash", "rm -rf a/b", true},
		{"slash segment of a compound matches", []string{"bash:rm *"}, "bash", "git status && rm -rf a/b", true},
		{"legacy first-token shape still matches", []string{"bash:git"}, "bash", "git status", true},
		{"legacy raw first token still matches before wrapper peel", []string{"bash:timeout"}, "bash", "timeout 5 rm -rf a/b", true},
		{"legacy tool-name pattern still matches every bash call", []string{"bash"}, "bash", "ls", true},
		{"non-bash tool name still matches", []string{"read_file"}, "read_file", "", true},
		{"unparseable command still matches on its raw text", []string{"bash:/usr/bin/rm *"}, "bash", "/usr/bin/rm -rf a/b", true},
		{"unrelated pattern misses", []string{"bash:rm *"}, "bash", "mv a/b c/d", false},
		{"no patterns never match", nil, "bash", "rm -rf a/b", false},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			args := `{}`
			if tc.tool == "bash" {
				args = bashArgs(tc.cmd)
			}
			if got := matchesProtective(tc.patterns, tc.tool, args); got != tc.want {
				t.Errorf("matchesProtective(%v, %q, %q) = %v, want %v", tc.patterns, tc.tool, tc.cmd, got, tc.want)
			}
		})
	}
}

// TestMatchesPermissive covers smart mode's auto_approve matcher (change
// 0088): every legacy first-token allow still allows (compatibility), and the
// new per-segment subject allows only when EVERY segment matches strictly,
// with the same opaque-arg and redirect guards auto mode applies.
func TestMatchesPermissive(t *testing.T) {
	cases := []struct {
		desc     string
		patterns []string
		cmd      string
		want     bool
	}{
		{"legacy first-token allow is unchanged", []string{"bash:go"}, "go test ./...", true},
		{"legacy first-token allow still covers a compound (compatibility)", []string{"bash:git"}, "git status && rm -rf a/b", true},
		{"bash:git * now works in smart mode", []string{"bash:git *"}, "git status", true},
		{"every segment allowed allows", []string{"bash:git *"}, "git status && git log", true},
		{"strict: bash:git * misses a URL with slashes", []string{"bash:git *"}, "git clone https://x/y", false},
		{"strict: bash:git * does not launder a trailing rm", []string{"bash:git *"}, "git status && rm -rf /", false},
		{"strict: bash:rm build/* does not admit a parent escape", []string{"bash:rm build/*"}, "rm build/../x", false},
		{"an opaque operand cannot satisfy a scoping allow", []string{"bash:git *"}, "git clone $URL", false},
		{"a redirect never satisfies a segment allow", []string{"bash:echo *"}, "echo x > out.txt", false},
		{"no patterns never match", nil, "git status", false},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			if got := matchesPermissive(tc.patterns, "bash", bashArgs(tc.cmd)); got != tc.want {
				t.Errorf("matchesPermissive(%v, %q) = %v, want %v", tc.patterns, tc.cmd, got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: Write the failing smart-mode end-to-end tests**

Append to `internal/permissions/gate_test.go` (`newApproveRecorder` and `bashArgs` live in `auto_test.go`, same package):

```go
// TestSmartMode_SlashBearingAlwaysPromptDemotes is change 0088 end to end in
// smart mode: before, always_prompt bash:rm * missed rm -rf a/b (the legacy
// subject was only bash:rm), so auto_approve bash:rm ran it silently.
func TestSmartMode_SlashBearingAlwaysPromptDemotes(t *testing.T) {
	approve, called := newApproveRecorder(false)
	cfg := config.PermissionsConfig{
		Mode:         "smart",
		AutoApprove:  []string{"bash:rm"},
		AlwaysPrompt: []string{"bash:rm *"},
	}
	g := New(cfg, newTestRegistry("bash"), approve)
	res := g.Execute(context.Background(), "bash", bashArgs("rm -rf a/b"))
	if !*called {
		t.Fatal("always_prompt bash:rm * must demote rm -rf a/b to a prompt in smart mode")
	}
	if !res.IsError {
		t.Fatal("the recorder denied, so the call must not run")
	}
}

// TestSmartMode_AllowPatternsStayStrict proves the smart allow consumer does
// not widen across '/' or across segments (ADR-0005), and that bash:git * now
// approves a plain git command in smart mode as it does in auto.
func TestSmartMode_AllowPatternsStayStrict(t *testing.T) {
	cases := []struct {
		cmd         string
		autoApprove []string
		wantPrompt  bool
	}{
		{"git status", []string{"bash:git *"}, false},
		{"git clone https://x/y", []string{"bash:git *"}, true},
		{"git status && rm -rf /", []string{"bash:git *"}, true},
		{"rm build/../x", []string{"bash:rm build/*"}, true},
		{"go test ./...", []string{"bash:go"}, false}, // legacy fixture, unchanged
	}
	for _, tc := range cases {
		t.Run(tc.cmd, func(t *testing.T) {
			approve, called := newApproveRecorder(false)
			g := New(config.PermissionsConfig{Mode: "smart", AutoApprove: tc.autoApprove}, newTestRegistry("bash"), approve)
			g.Execute(context.Background(), "bash", bashArgs(tc.cmd))
			if *called != tc.wantPrompt {
				t.Errorf("smart %q with allow %v: prompted=%v, want %v", tc.cmd, tc.autoApprove, *called, tc.wantPrompt)
			}
		})
	}
}
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `go test ./internal/permissions/ -run 'TestMatchesProtective|TestMatchesPermissive|TestSmartMode_' -count=1`
Expected: FAIL to compile (`undefined: matchesProtective`, `undefined: matchesPermissive`).

- [ ] **Step 4: Implement the two matchers in `patterns.go`**

Replace `matchesAny` and its doc comment (lines 9-22) with the two functions below, and drop `"path"` from the import block (it becomes unused; keep `"encoding/json"` and `"strings"`). Leave `subjects` and `firstToken` unchanged.

```go
// matchesProtective reports whether any protective pattern (always_prompt in
// smart mode) matches a tool call (change 0088). It keeps the legacy
// {tool, "bash:<firstToken>"} subjects, so no rule that demoted a call before
// stops demoting it, and adds the one shared protective subject rule
// (protectiveHit: each parsed segment, its head, and the whole command) with
// the slash-spanning glob. A command the parser refuses is matched on its raw
// text instead, so a protective rule still reaches it.
func matchesProtective(patterns []string, toolName, args string) bool {
	if len(patterns) == 0 {
		return false
	}
	for _, subj := range subjects(toolName, args) {
		if matchesSubjects(consumerProtective, patterns, subj) {
			return true
		}
	}
	if toolName != "bash" {
		return false
	}
	command := bashCommand(args)
	segments, err := splitSegments(command)
	if err != nil {
		raw := strings.TrimSpace(command)
		return raw != "" && matchesSubjects(consumerProtective, patterns, "bash:"+raw)
	}
	return protectiveHit(patterns, segments)
}

// matchesPermissive reports whether an allow pattern (auto_approve in smart
// mode) admits a tool call (change 0088). The legacy {tool,
// "bash:<firstToken>"} match is kept for compatibility, so nothing that
// allowed before stops allowing. It additionally admits a bash command when
// EVERY parsed segment matches strictly, through the same allSegmentsAllowed
// auto mode uses (path.Match, opaque-arg and redirect guards, no allow_push),
// so "bash:git *" means the same thing in both modes and never crosses '/'.
func matchesPermissive(patterns []string, toolName, args string) bool {
	if len(patterns) == 0 {
		return false
	}
	for _, subj := range subjects(toolName, args) {
		if matchesSubjects(consumerPermissive, patterns, subj) {
			return true
		}
	}
	if toolName != "bash" {
		return false
	}
	segments, err := splitSegments(bashCommand(args))
	if err != nil || len(segments) == 0 {
		return false
	}
	return allSegmentsAllowed(segments, patterns, false)
}
```

- [ ] **Step 5: Wire the smart branch in `gate.go`**

In `resolve`, smart branch (lines 563-565), change only the two calls:

```go
		if !matchesProtective(g.cfg.AlwaysPrompt, name, args) {
			// auto_approve config patterns promote beyond the safe list.
			if matchesPermissive(g.cfg.AutoApprove, name, args) || onSafeList(name) {
```

Then confirm nothing else references the removed helper: `grep -rn "matchesAny(" internal/` must print nothing (`matchesAnyGlob` in `fetchhost.go` is a different function and stays).

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./internal/permissions/ -run 'TestMatchesProtective|TestMatchesPermissive|TestSmartMode_|TestAutoApprovePatternPromotes|TestAlwaysPrompt|TestBlackboard' -count=1 -v`
Expected: PASS. The pre-existing smart tests (`TestAutoApprovePatternPromotes`, `TestAlwaysPromptDemotesSafeList`, `TestBlackboardWriteDemotableViaAlwaysPrompt`) must pass unmodified.

- [ ] **Step 7: Full package check and commit**

Run: `gofmt -l internal/permissions && go vet ./internal/permissions/ && go test ./internal/permissions/... -count=1`
Expected: clean, all PASS.

```bash
git add internal/permissions/patterns.go internal/permissions/patterns_test.go internal/permissions/gate.go internal/permissions/gate_test.go
git commit -m "permissions: give smart mode the same pattern subjects as auto

Smart always_prompt now goes through protectiveHit (per segment, head,
whole command, slash-spanning glob) on top of its legacy first-token
subjects. Smart auto_approve keeps its first-token match and also
admits a command when every segment matches strictly, so bash:git *
works in both modes and still never crosses a slash (change 0088)."
```

---

### Task 4: Smart/auto parity table

**Suggested tier:** standard

**Files:**
- Create: `internal/permissions/patternparity_test.go`

This task adds tests only; the behavior already exists after Tasks 2 and 3. Per the learnings ledger ("parity test feeds each side its own production source"), each mode is driven from its own real entry point: smart receives the raw JSON tool args through `matchesProtective` and the gate; auto receives `splitSegments` output through `protectiveHit`, `evalRules` and the gate. No shared pre-parsed fixture feeds both sides.

- [ ] **Step 1: Write the parity test**

Create `internal/permissions/patternparity_test.go`:

```go
package permissions

import (
	"context"
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
)

// protectiveParityRows is the one deny/ask table change 0088 runs through
// smart and auto mode. witness is an allow set that approves cmd in BOTH
// modes when no protective pattern is configured, so a prompt (or a deny)
// proves the protective pattern fired. Every cmd is off the read-only
// safelist, so no later auto-mode layer can mask a hit on this base (an
// auto-mode ask is not terminal until change 0087).
var protectiveParityRows = []struct {
	desc    string
	pattern string
	cmd     string
	witness []string
	hit     bool
}{
	{"star spans a slash argument", "bash:rm *", "rm -rf a/b", []string{"bash:rm -rf a/*"}, true},
	{"star spans the slash segment of a compound", "bash:rm *", "git status && rm -rf a/b", []string{"bash:git status", "bash:rm -rf a/*"}, true},
	{"bare head matches in both modes", "bash:rm", "rm -rf a/b", []string{"bash:rm -rf a/*"}, true},
	{"bash:git * matches a slash-bearing git command", "bash:git *", "git commit -m x/y", []string{"bash:git commit -m x/*"}, true},
	{"trailing star spans a path", "bash:npm publish*", "npm publish ./pkg/dir", []string{"bash:npm publish ./pkg/*"}, true},
	{"class plus star", "bash:rm -[rR]f *", "rm -Rf a/b", []string{"bash:rm -Rf a/*"}, true},
	{"question mark matches a slash", "bash:rm -rf a?b", "rm -rf a/b", []string{"bash:rm -rf a/*"}, true},
	{"whole-command pattern", "bash:git status*rm -rf a/*", "git status && rm -rf a/b", []string{"bash:git status", "bash:rm -rf a/*"}, true},
	{"unrelated pattern misses", "bash:rm *", "mv a/b c/d", []string{"bash:mv a/* c/*"}, false},
	{"malformed pattern matches nothing", "bash:rm [", "rm -rf a/b", []string{"bash:rm -rf a/*"}, false},
}

// TestProtectivePatterns_SameMatchInSmartAndAuto checks the matcher layer:
// smart's matchesProtective (raw JSON args) and auto's protectiveHit (parsed
// segments) agree with each other and with the expected hit.
func TestProtectivePatterns_SameMatchInSmartAndAuto(t *testing.T) {
	for _, r := range protectiveParityRows {
		t.Run(r.desc, func(t *testing.T) {
			smart := matchesProtective([]string{r.pattern}, "bash", bashArgs(r.cmd))
			auto := protectiveHit([]string{r.pattern}, segsFor(t, r.cmd))
			if smart != r.hit || auto != r.hit {
				t.Errorf("%q vs %q: smart=%v auto=%v, want %v in both", r.pattern, r.cmd, smart, auto, r.hit)
			}
		})
	}
}

// TestProtectivePatterns_SameVerdictInSmartAndAuto is the end-to-end parity
// check through the real gate. For each row: with only the witness allow,
// neither mode prompts (the discriminator works); with the pattern as
// always_prompt, both modes prompt exactly when it hits, and auto.ask agrees;
// with the pattern as auto.deny, auto denies without a prompt exactly when it
// hits. Smart mode has no deny consumer, so its deny-shaped rule is the
// always_prompt row.
func TestProtectivePatterns_SameVerdictInSmartAndAuto(t *testing.T) {
	for _, r := range protectiveParityRows {
		t.Run(r.desc, func(t *testing.T) {
			run := func(cfg config.PermissionsConfig) (prompted, failed bool) {
				approve, called := newApproveRecorder(false)
				g := New(cfg, newTestRegistry("bash"), approve, WithWorkspaceRoot(t.TempDir()))
				res := g.Execute(context.Background(), "bash", bashArgs(r.cmd))
				return *called, res.IsError
			}
			smartCfg := func(alwaysPrompt []string) config.PermissionsConfig {
				return config.PermissionsConfig{Mode: "smart", AutoApprove: r.witness, AlwaysPrompt: alwaysPrompt}
			}

			if p, _ := run(smartCfg(nil)); p {
				t.Fatalf("baseline: smart witness %v must approve %q without a prompt", r.witness, r.cmd)
			}
			if p, _ := run(autoCfg(config.AutoConfig{}, r.witness, nil)); p {
				t.Fatalf("baseline: auto witness %v must approve %q without a prompt", r.witness, r.cmd)
			}

			smartPrompt, _ := run(smartCfg([]string{r.pattern}))
			autoPrompt, _ := run(autoCfg(config.AutoConfig{}, r.witness, []string{r.pattern}))
			autoAsk, _ := run(autoCfg(config.AutoConfig{Ask: []string{r.pattern}}, r.witness, nil))
			if smartPrompt != r.hit || autoPrompt != r.hit || autoAsk != r.hit {
				t.Errorf("ask parity for %q vs %q: smart always_prompt=%v auto always_prompt=%v auto.ask=%v, want %v",
					r.pattern, r.cmd, smartPrompt, autoPrompt, autoAsk, r.hit)
			}

			denyPrompt, denyFailed := run(autoCfg(config.AutoConfig{Deny: []string{r.pattern}}, r.witness, nil))
			if denyPrompt {
				t.Errorf("auto.deny %q must never route %q to the human", r.pattern, r.cmd)
			}
			if denyFailed != r.hit {
				t.Errorf("auto.deny %q on %q: denied=%v, want %v", r.pattern, r.cmd, denyFailed, r.hit)
			}
		})
	}
}
```

- [ ] **Step 2: Run the test and confirm it passes**

Run: `go test ./internal/permissions/ -run 'TestProtectivePatterns_' -count=1 -v`
Expected: PASS for every row. (This was prototyped against the Task 1-3 implementation: all rows behave as listed.)

Sanity-check that the test discriminates: temporarily change `consumerProtective` handling in `matchesSubjects` to call `path.Match` instead of `matchProtective`, rerun, and confirm the slash rows FAIL in both tests; then revert that temporary edit before committing (`git diff internal/permissions/rules.go` must be empty).

- [ ] **Step 3: Full package check and commit**

Run: `gofmt -l internal/permissions && go vet ./internal/permissions/ && go test ./internal/permissions/... -count=1`
Expected: clean, all PASS.

```bash
git add internal/permissions/patternparity_test.go
git commit -m "permissions: pin identical protective verdicts in smart and auto

One deny/ask table runs through each mode's own entry point, at the
matcher layer and end to end through the gate, with a witness allow
proving each prompt or deny comes from the protective pattern
(change 0088)."
```

---

### Task 5: Document the matching rule

**Suggested tier:** economy

**Files:**
- Modify: `README.md` (the `[permissions]` section, around lines 197-247)

- [ ] **Step 1: Update the config comments**

In the YAML block (around lines 197-203), change the three pattern comments to:

```yaml
  auto_approve: []      # per-segment allow patterns, strict glob (see below): "bash:git *", ...
  always_prompt: []     # protective patterns demoted to always-prompt, e.g. "bash:git push*"
```

and

```yaml
    deny: []            # extra always-deny protective patterns, e.g. "bash:npm publish*"
    ask: []             # extra always-ask protective patterns (override an allow)
```

Keep every other line of the block as is.

- [ ] **Step 2: Add the matching-rule paragraph**

Directly after the paragraph that begins `**\`auto_approve\` (and \`auto.deny\`/\`auto.ask\`) are per-segment, not first-token` (ends "...all fail closed rather than slip past a prefix match."), insert:

```markdown
**How patterns match (every mode alike).** A bash command is matched per
parsed segment against `bash:<argv0> <args>`, for example `bash:rm -rf a/b`.
Protective patterns (`always_prompt`, `auto.deny`, `auto.ask`) are also
matched against the segment head `bash:<argv0>` and against the whole command,
and their `*` matches any run of characters **including `/`**, so
`deny: ["bash:rm *"]` denies `rm -rf a/b` and `ask: ["bash:cat*"]` asks for
`cat dir/file`. Allow patterns (`auto_approve`) are strict: their `*` stops at
`/`, so `bash:git *` does not approve `git clone https://host/repo` and
`bash:rm build/*` does not approve `rm build/../x`, and every segment must
match on its own. `?` matches one character and `[...]` classes work as in a
shell glob; a malformed pattern matches nothing. Smart mode applies the same
rule and also keeps its older first-word match (`bash:git` still approves any
command that starts with `git`), so no existing smart-mode rule changes
meaning. Protective rules match generously because they only ever tighten;
allow rules match strictly because a match grants permission.
```

- [ ] **Step 3: Check and commit**

Run: `git diff --stat` and `git diff README.md | grep '^+' | grep -c $'\xe2\x80\x94'`
Expected: the diff touches only `README.md`, and the second command prints `0` (no em dash in the added lines; pre-existing README text is not touched).

```bash
git add README.md
git commit -m "docs: state the one permission-pattern matching rule

Protective patterns match per segment, head and whole command with a
slash-spanning star; allow patterns stay strict and per segment; smart
mode keeps its first-word match for compatibility (change 0088)."
```

---

## Final verification (run by docket-build's full-suite gate)

- `gofmt -l internal/permissions` prints nothing.
- `go vet ./...` is clean.
- `go test ./...` is green; in particular every pre-existing test in `internal/permissions` passes unmodified, which is the "existing allow fixtures produce byte-identical decisions" evidence together with `TestMatchesSubjects_PermissiveIsPathMatch`.
- `go test ./internal/permissions/ -run '^$' -fuzz FuzzMatchProtective -fuzztime 30s` finds nothing.

## Spec coverage

| Spec requirement | Task |
|---|---|
| `cmdglob.go` `matchProtective`: `*` spans `/`, `?` one char, path.Match classes, malformed matches nothing | 1 |
| `matchesSubjects` takes a consumer kind; protective uses `matchProtective`, permissive keeps `path.Match` | 2 |
| Per-segment `bash:<argv0> <args>`; protective also whole command and `bash:<argv0>` head | 2 (auto), 3 (smart) |
| Smart builds `{tool, bash:argv0, bash:segment}`, keeps first-token allow, gains full-segment subject | 3 |
| `bash:rm *` deny denies `rm -rf a/b` and `rm -rf a/b && ls` | 2, 4 |
| `bash:cat*` ask asks for `cat dir/file` | 2 (rules layer; see "Known behavior") |
| Allow `bash:git *` misses `git clone https://x/y` and `git status && rm -rf /` | 2 (auto), 3 (smart) |
| Allow `bash:rm build/*` does not allow `rm build/../x` | 2 (auto), 3 (smart) |
| Same deny/ask table in smart and auto, identical verdicts | 4 |
| Existing allow fixtures byte-identical | 2 (`TestMatchesSubjects_PermissiveIsPathMatch`), unchanged existing suite |
| Update user-facing permission docs | 5 |
| Record an ADR | Parent (not in this plan) |
