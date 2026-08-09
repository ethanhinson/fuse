package permissions

import (
	"path"
	"strings"

	"github.com/ethanhinson/fuse/internal/config"
)

// AutoConfig is the deny/ask pattern surface (and classifier alias) for auto
// mode, re-exported from the config package so rule evaluation reads as a
// self-contained permissions concern.
type AutoConfig = config.AutoConfig

// Verdict is the deterministic rule outcome for a (possibly compound) command.
type Verdict int

const (
	// VerdictAllow auto-approves without human involvement.
	VerdictAllow Verdict = iota
	// VerdictAsk routes to the human/fallback surface. This is the default:
	// an action no rule admits fails toward the human (CWE-1188 hardening),
	// never toward allow.
	VerdictAsk
	// VerdictDeny is a hard block from a deny/dangerous match. It wins over any
	// allow rule and over any session grant.
	VerdictDeny
)

// String renders the verdict as its lowercase rule keyword.
func (v Verdict) String() string {
	switch v {
	case VerdictAllow:
		return "allow"
	case VerdictAsk:
		return "ask"
	case VerdictDeny:
		return "deny"
	default:
		return "unknown"
	}
}

// dangerousNames are argv[0] basenames that force a hard deny regardless of any
// allow rule or session grant — they guard against over-broad grants. This is
// the coarse floor; the network-egress and read-only refinements land in later
// tasks. curl/wget are denied wholesale here as a coarse network-egress guard.
var dangerousNames = map[string]bool{
	"rm":       true,
	"chmod":    true,
	"chown":    true,
	"chgrp":    true,
	"kill":     true,
	"pkill":    true,
	"killall":  true,
	"dd":       true,
	"mkfs":     true,
	"truncate": true,
	"shutdown": true,
	"reboot":   true,
	"curl":     true,
	"wget":     true,
	// sudoedit (aka `sudo -e`) is a distinct executable from sudo that opens an
	// editor with elevated privileges. sudo itself is caught by the wrapper
	// guard, but sudoedit is a plain argv[0] and would otherwise be classified
	// as a normal command — so it must be denied here alongside the other
	// privilege/destruction vectors.
	"sudoedit": true,
}

// dangerousGitSubcommands are git subcommands that are dangerous even though
// bare git is not — matched on argv, never on basename alone.
var dangerousGitSubcommands = map[string]bool{
	"push":  true,
	"reset": true,
	"clean": true,
}

// evalRules resolves the deterministic verdict for the parsed segments of a
// command, applying deny-first / then-ask / then-allow precedence with a
// default of ask (deny-toward-human).
//
//   - deny wins globally: any segment matching a deny/dangerous pattern ⇒ deny.
//   - then ask: any segment matching an ask pattern (cfg.Ask + alwaysPrompt) ⇒ ask.
//   - then allow: only when EVERY segment matches an allow rule (autoApprove),
//     matched per-segment against the parsed argv — never the raw string.
//   - otherwise ask: an action no rule admits fails toward the human.
//
// Deny and ask patterns are matched against each segment's argv AND the whole
// command string; allow is matched per-segment only, so a whole-string allow
// can never rescue a dangerous segment.
func evalRules(segments []Segment, cfg AutoConfig, autoApprove, alwaysPrompt []string) Verdict {
	whole := wholeSubject(segments)

	// 1. Deny wins globally.
	for _, seg := range segments {
		if isDangerous(seg) {
			return VerdictDeny
		}
		if matchesSegment(cfg.Deny, seg) {
			return VerdictDeny
		}
	}
	if matchesSubjects(cfg.Deny, whole) {
		return VerdictDeny
	}

	// 2. Ask beats allow.
	askPatterns := append(append([]string(nil), cfg.Ask...), alwaysPrompt...)
	for _, seg := range segments {
		if matchesSegment(askPatterns, seg) {
			return VerdictAsk
		}
	}
	if matchesSubjects(askPatterns, whole) {
		return VerdictAsk
	}

	// 3. Allow only when every segment independently matches an allow rule.
	if len(segments) > 0 && allSegmentsAllowed(segments, autoApprove) {
		return VerdictAllow
	}

	// 4. Default: fail toward the human.
	return VerdictAsk
}

// allSegmentsAllowed reports whether every segment matches an allow pattern.
// An empty segment set is not allowed by omission (the caller guards len>0).
func allSegmentsAllowed(segments []Segment, autoApprove []string) bool {
	for _, seg := range segments {
		if !matchesSegment(autoApprove, seg) {
			return false
		}
	}
	return true
}

// isDangerous reports whether a segment is on the built-in dangerous list,
// including subcommand-qualified git forms.
func isDangerous(seg Segment) bool {
	if dangerousNames[seg.Name] {
		return true
	}
	if seg.Name == "git" && len(seg.Args) > 0 && dangerousGitSubcommands[seg.Args[0]] {
		return true
	}
	return false
}

// readOnlyUtils are argv[0] basenames that are read-only with ANY arguments —
// they have no mutating mode worth flag-inspecting. env is deliberately absent:
// it is already fail-closed by the parser as an arbitrary-arg wrapper, so it
// never reaches here.
var readOnlyUtils = map[string]bool{
	"ls":       true,
	"cat":      true,
	"pwd":      true,
	"wc":       true,
	"head":     true,
	"tail":     true,
	"grep":     true,
	"rg":       true,
	"echo":     true,
	"which":    true,
	"dirname":  true,
	"basename": true,
	"date":     true,
	"true":     true,
	"false":    true,
	"test":     true,
}

// readOnlyGitSubcommands are git subcommands that are always read-only (no
// mutating flag form worth inspecting). branch/remote/config/tag are handled
// separately because they have both read and mutating forms.
var readOnlyGitSubcommands = map[string]bool{
	"status":    true,
	"log":       true,
	"diff":      true,
	"show":      true,
	"rev-parse": true,
	"describe":  true,
	"blame":     true,
}

// findMutatingActions are find primaries that execute a command or mutate the
// filesystem. Any of them makes a find invocation unsafe.
var findMutatingActions = map[string]bool{
	"-exec":    true,
	"-execdir": true,
	"-delete":  true,
	"-fprint":  true,
	"-fprintf": true,
	"-ok":      true,
	"-okdir":   true,
}

// allSegmentsReadOnlySafe reports whether EVERY segment is independently on the
// built-in read-only safe list. An empty segment set is not safe (nothing to
// prove safe). This is the per-segment AND that lets Task 7's gate short-circuit
// a wholly read-only command to allow; a single unsafe segment sinks the whole.
func allSegmentsReadOnlySafe(segments []Segment) bool {
	if len(segments) == 0 {
		return false
	}
	for _, seg := range segments {
		if !isReadOnlySafe(seg) {
			return false
		}
	}
	return true
}

// isReadOnlySafe classifies a single parsed segment against the built-in
// read-only bash allowlist with flag-inspecting conditionals (the Codex
// is_safe_command shape). It fails toward unsafe: any argv[0] not enumerated,
// or any enumerated command in a form not provably read-only, is unsafe.
//
// The parser (splitSegments) guarantees seg.Name is a bare basename and that no
// path-qualified argv[0] (./sed) or arbitrary-arg wrapper ever reaches here.
func isReadOnlySafe(seg Segment) bool {
	if readOnlyUtils[seg.Name] {
		return true
	}
	switch seg.Name {
	case "find":
		return isSafeFind(seg.Args)
	case "git":
		return isSafeGit(seg.Args)
	case "sed":
		return isSafeSed(seg.Args)
	default:
		return false
	}
}

// isSafeFind reports whether a find invocation is read-only: it is safe unless
// it names a mutating/exec primary (-exec, -delete, …).
func isSafeFind(args []string) bool {
	for _, a := range args {
		if findMutatingActions[a] {
			return false
		}
	}
	return true
}

// isSafeGit reports whether a git invocation is a read-only subcommand form.
// When in doubt about a subcommand it returns false (fail toward the human) —
// only forms proven read-only are admitted.
func isSafeGit(args []string) bool {
	if len(args) == 0 {
		return false
	}
	sub := args[0]
	rest := args[1:]
	if readOnlyGitSubcommands[sub] {
		return true
	}
	switch sub {
	case "branch":
		return isSafeGitBranch(rest)
	case "remote":
		return isSafeGitRemote(rest)
	case "config":
		return isSafeGitConfig(rest)
	case "tag":
		return isSafeGitTag(rest)
	default:
		return false
	}
}

// isSafeGitBranch is safe only as a list: no args, or only list flags. Any
// non-flag argument (a branch name) or a mutating flag (-d/-D/-m/…) is unsafe.
func isSafeGitBranch(args []string) bool {
	listFlags := map[string]bool{"--list": true, "-l": true, "-a": true, "-r": true, "-v": true}
	for _, a := range args {
		if !listFlags[a] {
			return false
		}
	}
	return true
}

// isSafeGitRemote is safe as a bare listing, `-v`, or `show`; every mutating
// subcommand (add/remove/rename/set-url/…) is unsafe.
func isSafeGitRemote(args []string) bool {
	if len(args) == 0 {
		return true
	}
	switch args[0] {
	case "-v", "--verbose", "show":
		return true
	default:
		return false
	}
}

// isSafeGitConfig is safe only in the explicit read forms --get / --list;
// anything else (a bare `config key value` set) is unsafe.
func isSafeGitConfig(args []string) bool {
	for _, a := range args {
		if a == "--get" || a == "--get-all" || a == "--list" || a == "-l" {
			return true
		}
	}
	return false
}

// isSafeGitTag is safe only as a list: no args, or only the list flags -l /
// --list. A tag name (creation) or any other form is unsafe.
func isSafeGitTag(args []string) bool {
	for _, a := range args {
		if a != "-l" && a != "--list" {
			return false
		}
	}
	return true
}

// isSafeSed is read-only only in the non-mutating `-n …p` print form: it must
// carry -n (or a combined -n… flag) and must NOT carry -i (in-place). Any other
// form is unsafe (cannot be proven read-only).
func isSafeSed(args []string) bool {
	hasN := false
	for _, a := range args {
		if !strings.HasPrefix(a, "-") || a == "-" {
			continue // a script or file operand, not a flag.
		}
		if a == "--in-place" || strings.HasPrefix(a, "--in-place") {
			return false
		}
		if strings.HasPrefix(a, "--") {
			continue // some other long option; -n must still be proven below.
		}
		// A short-flag group like -n, -np, or -ni. Any 'i' in the group means
		// in-place (mutating) ⇒ unsafe; an 'n' contributes the print form.
		group := a[1:]
		if strings.ContainsRune(group, 'i') {
			return false
		}
		if strings.ContainsRune(group, 'n') {
			hasN = true
		}
	}
	return hasN
}

// matchesSegment matches patterns against a single segment's reconstructed
// bash subject line (e.g. "bash:git status"), per-segment against the parsed
// argv rather than the raw source text.
func matchesSegment(patterns []string, seg Segment) bool {
	return matchesSubjects(patterns, segmentSubject(seg))
}

// segmentSubject reconstructs the "bash:<argv joined>" subject for a segment,
// mirroring the legacy "bash:<firstToken>" shape used by auto_approve /
// always_prompt patterns but carrying the full argv so "bash:git *" matches.
func segmentSubject(seg Segment) string {
	line := seg.Name
	if len(seg.Args) > 0 {
		line += " " + strings.Join(seg.Args, " ")
	}
	return "bash:" + line
}

// wholeSubject reconstructs the "bash:<all segments>" subject spanning the
// whole command, used for deny/ask whole-string matching.
func wholeSubject(segments []Segment) string {
	lines := make([]string, 0, len(segments))
	for _, seg := range segments {
		lines = append(lines, strings.TrimPrefix(segmentSubject(seg), "bash:"))
	}
	return "bash:" + strings.Join(lines, " ")
}

// matchesSubjects matches patterns against a single subject using path.Match
// glob semantics — the same engine as the legacy smart path, but here the
// subject carries the full argv rather than only the first token.
func matchesSubjects(patterns []string, subject string) bool {
	for _, pat := range patterns {
		if ok, _ := path.Match(pat, subject); ok {
			return true
		}
	}
	return false
}
