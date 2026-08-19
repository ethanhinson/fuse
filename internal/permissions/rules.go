package permissions

import (
	"os"
	"path"
	"path/filepath"
	"strconv"
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
// allow rule or session grant. Change 0068 shrank this to genuinely
// CATASTROPHIC operations only (Claude Code / Cursor parity): machine-level
// destruction and privilege escalation. Everything removed (rm, chmod, chown,
// chgrp, kill, pkill, killall, dd, truncate, curl, wget) now flows to the
// shape-based denies below, the workspace-scoping heuristic, or the
// context-aware classifier — scoped-allow where provable, never a blind block.
var dangerousNames = map[string]bool{
	"mkfs":     true, // plus every mkfs.* variant via the prefix check in isDangerous
	"shutdown": true,
	"reboot":   true,
	"halt":     true,
	"poweroff": true,
	// sudoedit (aka `sudo -e`) is a distinct executable from sudo that opens an
	// editor with elevated privileges. sudo itself is caught by the wrapper
	// guard, but sudoedit is a plain argv[0] and would otherwise be classified
	// as a normal command — so it must be denied here alongside the other
	// privilege/destruction vectors.
	"sudoedit": true,
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
func evalRules(segments []Segment, cfg AutoConfig, autoApprove, alwaysPrompt []string, workspaceRoot string) Verdict {
	whole := wholeSubject(segments)

	// 1. Deny wins globally.
	for _, seg := range segments {
		if isDangerous(seg, workspaceRoot) {
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

	// 3. Allow only when every segment independently matches an allow rule (or
	// the config-opted git-push allowance, change 0068 — checked AFTER deny/ask
	// so a config deny or always_prompt still beats allow_push).
	if len(segments) > 0 && allSegmentsAllowed(segments, autoApprove, cfg.AllowPush) {
		return VerdictAllow
	}

	// 4. Default: fail toward the human.
	return VerdictAsk
}

// allSegmentsAllowed reports whether every segment matches an allow pattern —
// or is a git push under the trusted allow_push opt-in (change 0068). An empty
// segment set is not allowed by omission (the caller guards len>0).
func allSegmentsAllowed(segments []Segment, autoApprove []string, allowPush bool) bool {
	for _, seg := range segments {
		// A redirect write target is not in the subject an allow pattern matches
		// against (segmentSubject reconstructs argv only), and this layer — like
		// the safelist below it — runs with no root set, so it could not scope the
		// target even if it saw it. Consenting to `echo x` is not consenting to
		// `echo x > /etc/passwd`, so a redirected segment declines the pattern
		// allow and falls through to the scoping layer, which allows the in-root
		// case on its own evidence (change 0070 D4).
		if len(seg.WriteTargets) > 0 {
			return false
		}
		// An OPAQUE arg cannot satisfy a pattern either, for the same reason and
		// with a sharper edge (change 0070 D5). segmentSubject reconstructs the
		// subject from each word's raw SOURCE text, and allow patterns are matched
		// with path.Match, whose `*` does not cross `/`. That slash boundary is the
		// containment a human writing `bash:git *` or `bash:rm build/*` is relying
		// on — and an opaque word collapses the operand being scoped into a single
		// token with no `/` in it. `git clone https://evil.example/x` correctly
		// MISSES `bash:git *` and falls through to the egress boundary; `git clone
		// $URL` would MATCH it, and rules allow is terminal, so neither the egress
		// boundary nor the mutating-scope opaque ask would ever run.
		//
		// The raw text stays visible to the deny and always_prompt consumers above:
		// those only ever tighten, so matching more text there is safe. Only this
		// consumer turns a match into an authorization, so only this one has to
		// decline. The segment falls through to the scoping layer, which asks.
		if seg.hasOpaqueArg() {
			return false
		}
		if matchesSegment(autoApprove, seg) {
			continue
		}
		if allowPush && isGitPush(seg) {
			continue
		}
		return false
	}
	return true
}

// isGitPush reports whether a segment is a git push invocation.
//
// No opaque guard is needed (change 0070 D5): an opaque arg's stored text is
// the word's raw SOURCE ("$SUB", "pu$X"), which can never equal the literal
// "push", so `git $SUB` is not a git push here. Both consumers get the answer
// they want from that — isEgress falls through to the mutating scope where the
// opaque arg asks, and allSegmentsAllowed declines the allow_push allowance.
func isGitPush(seg Segment) bool {
	return seg.Name == "git" && len(seg.Args) > 0 && seg.Args[0] == "push"
}

// isDangerous reports whether a segment is catastrophic: on the built-in
// dangerous list (with mkfs.* prefix coverage) or matching a shape-based
// catastrophic pattern (rm -rf on /, $HOME, or a workspace ancestor; dd onto a
// raw device). workspaceRoot is the canonicalized workspace for the rm check.
func isDangerous(seg Segment, workspaceRoot string) bool {
	if dangerousNames[seg.Name] {
		return true
	}
	if strings.HasPrefix(seg.Name, "mkfs.") {
		return true
	}
	if isCatastrophicRm(seg, workspaceRoot) {
		return true
	}
	if isDdToDevice(seg) {
		return true
	}
	return false
}

// isCatastrophicRm reports whether an rm segment is machine-destroying: it
// carries a recursive or force flag (short-grouped or long form) AND any
// operand resolves to the filesystem root, the user's home directory, or an
// ancestor of the workspace root. Plain rm (or rm -rf of anything else) falls
// through to the workspace-scoping heuristic: in-workspace ⇒ allow, outside ⇒
// classifier — consistent with every other in-workspace mutation.
func isCatastrophicRm(seg Segment, workspaceRoot string) bool {
	if seg.Name != "rm" {
		return false
	}
	forceish := false
	var operands []string
	for i, a := range seg.Args {
		// An OPAQUE word is neither a provable flag nor a provable operand
		// (change 0070 D5), and it must not become one. This function calls
		// resolveExisting on every operand — the exact
		// walk-up-to-the-deepest-existing-ancestor the D5 invariant forbids over
		// a word that is not a path. Feeding it "$VAR" would resolve to
		// <cwd>/$VAR and answer the "is this / or $HOME or a workspace
		// ancestor?" question about a path that does not exist and was never
		// asked about. Skipping keeps the DENY honest: `rm -rf $VAR` is not
		// provably catastrophic, so it is not denied here — it is unprovable,
		// and classifyHeuristic asks.
		if seg.isOpaque(i) {
			continue
		}
		switch {
		case a == "--recursive" || a == "--force":
			forceish = true
		case strings.HasPrefix(a, "--"):
			// other long flags: not force-ish, not operands
		case strings.HasPrefix(a, "-") && len(a) > 1:
			if strings.ContainsAny(a[1:], "rRf") {
				forceish = true
			}
		case a != "":
			operands = append(operands, a)
		}
	}
	if !forceish {
		return false
	}
	home, _ := os.UserHomeDir()
	for _, op := range operands {
		// The parser hands tilde through literally; bash expands it at execution
		// (`rm -rf ~` destroys the home directory, not a cwd file named "~").
		if home != "" && (op == "~" || strings.HasPrefix(op, "~/")) {
			op = home + op[1:]
		}
		abs, err := filepath.Abs(op)
		if err != nil {
			continue
		}
		resolved, ok := resolveExisting(abs)
		if !ok {
			continue
		}
		if resolved == "/" {
			return true
		}
		if home != "" && resolved == home {
			return true
		}
		// An ancestor of the workspace (the workspace's parent chain): deleting
		// it recursively takes the workspace with it.
		if workspaceRoot != "" && resolved != workspaceRoot && isWithin(workspaceRoot, resolved) {
			return true
		}
	}
	return false
}

// isDdToDevice reports whether a dd segment writes to a raw device
// (of=/dev/...): destroying a disk is catastrophic regardless of scoping.
// Other dd targets flow to the heuristic via pathArgs' of=/if= extraction.
//
// OPAQUE operands are deliberately NOT skipped here, unlike in isCatastrophicRm
// (change 0070 D5). The difference is what the two functions do with the text:
// this one only pattern-matches a prefix and never resolves a path, so the D5
// invariant is not in play. A mixed word like `of=/dev/$DISK` therefore still
// denies — the unresolvable half is the device NAME, and every value it could
// take is a raw device. Erring toward the deny is right for a disk-destroying
// shape; a fully opaque `of=$OUT` matches nothing here and asks one layer down.
func isDdToDevice(seg Segment) bool {
	if seg.Name != "dd" {
		return false
	}
	for _, a := range seg.Args {
		if strings.HasPrefix(a, "of=/dev/") {
			return true
		}
	}
	return false
}

// isProvablyBenignKill reports whether a kill segment provably targets specific
// processes by numeric PID (never PID 1) with at most signal flags — the
// dominant "restart my dev server" shape, deterministically allowable. Anything
// else (pkill/killall name matching, %jobspec, flags it can't prove) is not
// benign and routes to the classifier via the heuristic layer.
func isProvablyBenignKill(seg Segment) bool {
	if seg.Name != "kill" {
		return false
	}
	// An OPAQUE operand is not a provable numeric PID (change 0070 D5): `kill
	// $PID` could be any process on the machine, and `kill -$SIG 1` could be
	// any signal. strconv.Atoi("$PID") already fails, but relying on that would
	// leave the guard resting on an accident of the token's text — an opaque
	// word whose raw source begins with "-" would be silently swallowed by the
	// signal-flag arm below.
	if seg.hasOpaqueArg() {
		return false
	}
	sawPID := false
	for i := 0; i < len(seg.Args); i++ {
		a := seg.Args[i]
		switch {
		case a == "-s" || a == "--signal":
			i++ // consume the signal name
		case strings.HasPrefix(a, "-") && len(a) > 1:
			// -9, -TERM, -SIGKILL … : a signal flag. Numeric-PID flags like -1
			// (broadcast) are indistinguishable from signal -1 (SIGHUP); kill's
			// own semantics make a leading -N a signal, so this stays a flag.
		case a == "":
			continue
		default:
			pid, err := strconv.Atoi(a)
			if err != nil || pid <= 1 {
				return false
			}
			sawPID = true
		}
	}
	return sawPID
}

// readOnlyUtils are argv[0] basenames that are read-only with ANY arguments —
// they have no mutating mode worth flag-inspecting. env is deliberately absent
// because it can never reach here as a segment Name: the parser either peels it
// away — emitting the inner command as the segment, or no segment at all for a
// bare `env` that merely prints the environment — or fails the whole command
// closed (change 0070 D2, peelEnv in shellparse.go). Listing it would not make
// `env <cmd>` read-only safe; it would only add a name no segment can carry.
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
//
// A segment carrying a redirect write target (change 0070 D4) is never wholly
// read-only here, however read-only its command is. The reason is specifically
// about WHERE this function is consulted: the safelist short-circuit at
// gate.go:660 runs with NO root context, so it cannot scope `> /etc/passwd`
// against anything — calling `echo x > /etc/passwd` read-only safe would
// auto-approve the write right there and it would never reach a layer that
// could catch it. Redirected commands must fall through to the heuristic layer,
// which holds `roots` and allows an in-root target deterministically.
func allSegmentsReadOnlySafe(segments []Segment) bool {
	if len(segments) == 0 {
		return false
	}
	for _, seg := range segments {
		if len(seg.WriteTargets) > 0 {
			return false
		}
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
	// A readOnlyUtils name is read-only with ANY arguments — there is no
	// mutating mode to flag-inspect — so an OPAQUE argument changes nothing.
	// `cat $F` reads unknown data, and reading is not a mutation (change 0070
	// D5). This is the widening D5 buys: before it, the whole command failed
	// closed and a human was asked about `wc -l $f`.
	if readOnlyUtils[seg.Name] {
		return true
	}
	// The FLAG-INSPECTING names are the opposite case, and the line matters.
	// isSafeFind, isSafeGit and isSafeSed all decide by READING the argument
	// words: they look for -exec/-delete, for a mutating subcommand, for -i.
	// An opaque word could BE any of those — `sed $F x` is `sed -i x` if $F
	// happens to be "-i" — so the inspection is no longer a proof and the
	// segment fails toward unsafe. It then reaches classifyHeuristic, where the
	// opaque arg asks. A single blanket check rather than three, so a future
	// flag-inspecting name added to the switch inherits it by default.
	if seg.hasOpaqueArg() {
		return false
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
//
// An OPAQUE argument contributes its RAW SOURCE TEXT ("$VAR", "$(git
// rev-parse)") — that is the whole reason the parser keeps the text at all
// (change 0070 D5). Deny and always_prompt patterns should see the word a human
// wrote, and a decision reason naming `rm $VAR` is readable where a blank would
// not be. This is safe because the subject is only ever glob-MATCHED, never
// resolved: the deny/ask uses tighten, and the allow use requires a
// human-authored auto_approve pattern that already names the command
// (auto_approve is empty by default and is a trust-boundary key local config
// cannot loosen). It is emphatically NOT a value — nothing may read it back out
// of this string and treat it as a path.
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
