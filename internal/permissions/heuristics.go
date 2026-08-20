package permissions

import (
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// classifyHeuristic is the heuristic permission layer: it classifies a parsed
// command into a Verdict the gate (Task 7) can consume, composing three
// concerns in strict order.
//
//  1. Network egress is a hard risk boundary. Any egress segment (curl, wget,
//     nc, ssh, scp, or a host-qualified git remote) ⇒ VerdictAsk. It is checked
//     first so an egress command can never fall through to a silent allow.
//  2. Wholly read-only commands ⇒ VerdictAllow. A read touches nothing outside
//     the process, so no path scoping is required — the existing safe-list
//     classifier (allSegmentsReadOnlySafe) decides.
//  3. Otherwise the command mutates. Every path argument of every mutating
//     segment is resolved symlink-aware and must remain within workspaceRoot.
//     An escape (a path resolving outside the root) ⇒ VerdictAsk; only a fully
//     in-scope mutation ⇒ VerdictAllow. Anything the heuristic cannot prove
//     in-scope fails toward the human (VerdictAsk), never toward allow.
//
// roots is the allowed mutation-root set: the canonicalized workspace root
// first, then any extra write roots (the per-session scratch dir, config
// write_roots — change 0068). Every root must already be canonicalized
// (filepath.EvalSymlinks) by the caller so containment comparisons are made
// against real paths.
func classifyHeuristic(segments []Segment, roots []string) Verdict {
	if len(segments) == 0 {
		return VerdictAsk
	}

	// 1. Egress boundary wins first — never a silent allow. A curl/wget whose
	// every URL is provably loopback (and whose outputs land in-roots) is the
	// dev-server health-check shape, not egress (change 0068).
	for _, seg := range segments {
		if isEgress(seg) && !isLoopbackFetch(seg, roots) {
			return VerdictAsk
		}
	}

	// 2. Wholly read-only ⇒ allow, no scoping needed.
	if allSegmentsReadOnlySafe(segments) {
		return VerdictAllow
	}

	// 3. Mutating: every path argument must stay inside the allowed roots.
	for _, seg := range segments {
		// A redirect target is a write no matter how read-only the command in
		// front of it is (`echo x > /etc/passwd`) and no matter which of the
		// exemptions below the segment qualifies for — the kill family's operands
		// are PIDs, and a loopback fetch's operands are URLs, but neither makes
		// their `> file` anything other than a file write. Scoping the targets
		// FIRST is therefore load-bearing: every `continue` after this point
		// would carry them past unscoped (change 0070 D4).
		for _, target := range seg.WriteTargets {
			if !withinAnyRoot(target, roots) {
				return VerdictAsk
			}
		}
		if isReadOnlySafe(seg) {
			continue // a read-only segment mutates nothing else to scope.
		}

		// THE INVARIANT of change 0070 D5, and the single most important line in
		// it. This segment MUTATES and carries at least one opaque argument — a
		// word whose value is not statically known ($VAR, $(…)). There is
		// nothing to prove containment over, so the answer is the human.
		//
		// Read this together with pathArgs, which drops opaque positions so an
		// opaque token can never reach withinWorkspace/resolveExisting. THAT
		// GUARD ALONE IS A WORSE BYPASS THAN NEITHER: with the token dropped,
		// `rm $(echo /)` arrives here with ZERO path args, the scoping loop
		// below finds nothing to fail on, and the function returns
		// VerdictAllow — auto-approving `rm /`. The drop removes the false
		// PROOF; this line supplies the missing verdict.
		//
		// It sits ahead of the kill and loopback-fetch exemptions on purpose:
		// both of those are proofs about operands ("these are numeric PIDs",
		// "these are loopback URLs"), and an operand we could not resolve is not
		// an operand either of them may claim to have inspected.
		if seg.hasOpaqueArg() {
			return VerdictAsk
		}
		// The kill family's operands are PIDs or process names, not paths —
		// path-scoping them against the cwd would prove nothing (change 0068).
		// A provably benign kill (numeric PIDs, signal flags only) allows;
		// everything else in the family routes to the classifier.
		switch seg.Name {
		case "kill":
			if isProvablyBenignKill(seg) {
				continue
			}
			return VerdictAsk
		case "pkill", "killall":
			return VerdictAsk
		case "docker":
			// The read-only docker forms were admitted by the isReadOnlySafe
			// continue above; everything else docker reaches here. Its operands
			// are images, containers, services, and volumes — NAMES, not paths
			// (change 0072, same lesson as the kill family). Path-scoping them
			// would resolve "compose"/"up"/"data" as cwd-relative words that
			// trivially prove in-workspace, deterministically allowing
			// `docker volume rm data` or `docker system prune -f` with no
			// human. Nothing mutating may pass this layer; the classifier owns
			// docker's gray area.
			return VerdictAsk
		}
		if isLoopbackFetch(seg, roots) {
			continue // URL operands are not paths; outputs were scoped inside.
		}
		for _, arg := range pathArgs(seg) {
			if !withinAnyRoot(arg, roots) {
				return VerdictAsk
			}
		}
	}
	return VerdictAllow
}

// withinAnyRoot reports whether arg, resolved symlink-aware, stays within any
// of the allowed roots (change 0068). The empty-roots case is never within.
func withinAnyRoot(arg string, roots []string) bool {
	for _, root := range roots {
		if root != "" && withinWorkspace(arg, root) {
			return true
		}
	}
	return false
}

// egressNames are argv[0] basenames that open a network connection to an
// arbitrary host. Their egress is a hard risk boundary regardless of arguments.
var egressNames = map[string]bool{
	"curl":  true,
	"wget":  true,
	"nc":    true,
	"ncat":  true,
	"ssh":   true,
	"scp":   true,
	"sftp":  true,
	"rsync": true,
}

// gitEgressSubcommands are git subcommands that reach a remote when handed a
// host-qualified URL argument (as opposed to a local remote name).
var gitEgressSubcommands = map[string]bool{
	"clone": true,
	"fetch": true,
	"pull":  true,
	"push":  true,
}

// isEgress reports whether a segment performs network egress: a known egress
// tool, or a git remote operation given a host-qualified URL. `git push` is
// egress UNCONDITIONALLY (change 0068): its usual operands (`origin`, a branch
// name) resolve under the cwd and would otherwise slip through the mutating
// path scope as in-workspace, silently allowing a publish. clone/fetch/pull
// keep the host-qualified requirement (a bare `git pull` against a configured
// remote is routine sync, and its worst case pulls INTO the workspace).
func isEgress(seg Segment) bool {
	if egressNames[seg.Name] {
		return true
	}
	if isGitPush(seg) {
		return true
	}
	if seg.Name == "git" && len(seg.Args) > 0 && !seg.isOpaque(0) && gitEgressSubcommands[seg.Args[0]] {
		for i, a := range seg.Args[1:] {
			// An OPAQUE remote argument counts as host-qualified (change 0070
			// D5): isHostQualified inspects the text for "://", "@" and ":"
			// markers, and "$REMOTE" carries none of them — so `git clone $URL`
			// would read as a LOCAL remote name and skip the egress boundary
			// entirely. Unprovable means we take the stricter reading.
			if seg.isOpaque(i+1) || isHostQualified(a) {
				return true
			}
		}
	}
	return false
}

// isLoopbackHost reports whether host is the loopback interface: the literal
// "localhost" or an IP with IsLoopback. Deliberately narrower than the
// web_fetch SSRF check (no RFC-1918/link-local): only the machine's own
// services qualify for the deterministic curl allow (change 0068).
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// curl/wget flag tables for isLoopbackFetch. A flag outside these tables makes
// the segment unprovable (⇒ not a loopback fetch ⇒ egress ask ⇒ classifier):
// curl has many flags that write files (-c, -D, --trace, -K …) and enumerating
// the benign set is the only fail-closed posture.
var (
	curlFlagsNoValue = map[string]bool{
		"-s": true, "--silent": true, "-S": true, "--show-error": true,
		"-f": true, "--fail": true, "-L": true, "--location": true,
		"-v": true, "--verbose": true, "-i": true, "--include": true,
		"-I": true, "--head": true, "-k": true, "--insecure": true,
		"-4": true, "-6": true, "-N": true, "--no-buffer": true,
		"-g": true, "--globoff": true, "--compressed": true,
		"-O": true, "--remote-name": true, // writes basename into the cwd (the workspace)
	}
	curlFlagsWithValue = map[string]bool{
		"-X": true, "--request": true, "-H": true, "--header": true,
		"-d": true, "--data": true, "--data-raw": true, "--data-binary": true,
		"-m": true, "--max-time": true, "--connect-timeout": true,
		"-w": true, "--write-out": true, "-u": true, "--user": true,
		"--retry": true, "-A": true, "--user-agent": true,
	}
	curlFlagsWithScopedValue = map[string]bool{
		"-o": true, "--output": true,
	}
	wgetFlagsNoValue = map[string]bool{
		"-q": true, "--quiet": true, "-nv": true, "--no-verbose": true,
		"-4": true, "-6": true, "--no-check-certificate": true,
	}
	wgetFlagsWithValue = map[string]bool{
		"-T": true, "--timeout": true, "-t": true, "--tries": true,
		"-U": true, "--user-agent": true, "--header": true,
		"--post-data": true, "--method": true,
	}
	wgetFlagsWithScopedValue = map[string]bool{
		"-O": true, "--output-document": true, "-P": true, "--directory-prefix": true,
	}
)

// isLoopbackFetch reports whether a curl/wget segment is provably a loopback
// fetch (change 0068): every URL-shaped operand's host is loopback, every
// output-writing flag's target stays within the allowed roots, and every flag
// is on the known-benign table. Anything unprovable — a non-loopback or
// non-literal URL, an unknown flag, a missing flag value — returns false and
// the segment keeps its egress-ask ⇒ classifier routing.
func isLoopbackFetch(seg Segment, roots []string) bool {
	// An OPAQUE operand anywhere sinks the proof (change 0070 D5). This whole
	// function is an argument-by-argument PROOF, and every arm of the loop below
	// reads a word's text as a value — as a known flag, as a flag's scoped
	// output path, or as a URL whose host must be loopback. An opaque word is
	// none of those: `curl $URL` fetches an arbitrary host, `curl -o $OUT …`
	// writes an arbitrary file, and urlHost would cheerfully parse a "hostname"
	// out of a token that is not a URL at all. `curl $URL` must not inherit the
	// loopback allow.
	if seg.hasOpaqueArg() {
		return false
	}
	var noVal, withVal, scopedVal map[string]bool
	switch seg.Name {
	case "curl":
		noVal, withVal, scopedVal = curlFlagsNoValue, curlFlagsWithValue, curlFlagsWithScopedValue
	case "wget":
		noVal, withVal, scopedVal = wgetFlagsNoValue, wgetFlagsWithValue, wgetFlagsWithScopedValue
	default:
		return false
	}
	sawURL := false
	for i := 0; i < len(seg.Args); i++ {
		a := seg.Args[i]
		switch {
		case a == "":
			continue
		case noVal[a]:
			continue
		case withVal[a]:
			i++ // consume the value
			if i >= len(seg.Args) {
				return false
			}
		case scopedVal[a]:
			i++
			if i >= len(seg.Args) || !withinAnyRoot(seg.Args[i], roots) {
				return false
			}
		case strings.HasPrefix(a, "--"):
			return false // unknown long flag ⇒ unprovable
		case strings.HasPrefix(a, "-") && len(a) > 1:
			// A grouped short form (curl -sL): provable only when every letter is
			// itself a known no-value short flag.
			if !allShortFlagsNoValue(a[1:], noVal) {
				return false
			}
		case strings.HasPrefix(a, "-"):
			return false // bare "-" ⇒ unprovable
		default:
			host, ok := urlHost(a)
			if !ok || !isLoopbackHost(host) {
				return false
			}
			sawURL = true
		}
	}
	return sawURL
}

// allShortFlagsNoValue reports whether every letter of a grouped short flag is
// individually a known no-value flag ("-sL" ⇒ "-s" and "-L").
func allShortFlagsNoValue(letters string, noVal map[string]bool) bool {
	for _, r := range letters {
		if !noVal["-"+string(r)] {
			return false
		}
	}
	return true
}

// urlHost extracts the hostname from a URL-shaped operand, accepting both
// scheme URLs (http://localhost:4000/x) and the schemeless host[:port][/path]
// form curl accepts (localhost:4000/v1/models). ok is false for anything that
// does not parse to a non-empty host.
func urlHost(arg string) (string, bool) {
	s := arg
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Hostname() == "" {
		return "", false
	}
	// Only http(s) shapes qualify; anything else (file://, ftp://, gopher://)
	// is not the loopback health-check shape.
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", false
	}
	return u.Hostname(), true
}

// isHostQualified reports whether a git remote argument names a host rather than
// a local remote name — a scheme URL (https://…, git://…, ssh://…) or an
// scp-style host:path / user@host:path. A bare token with no host marker is a
// local remote name (origin) and is not egress by itself.
func isHostQualified(arg string) bool {
	if strings.HasPrefix(arg, "-") {
		return false // a flag, not a remote.
	}
	if strings.Contains(arg, "://") {
		return true
	}
	if strings.Contains(arg, "@") && strings.Contains(arg, ":") {
		return true // user@host:path
	}
	// scp-style host:path — a colon before any slash, with a non-empty host.
	if i := strings.IndexByte(arg, ':'); i > 0 {
		slash := strings.IndexByte(arg, '/')
		if slash == -1 || i < slash {
			return true
		}
	}
	return false
}

// pathArgs returns the non-flag operands of a mutating segment that are treated
// as filesystem paths to be scoped. Flags (leading '-') and their nature are
// not inspected individually; instead every non-flag word is conservatively
// scoped — a word that is not actually a path but happens to resolve outside the
// workspace only costs a human prompt, never a bypass.
//
// OPAQUE POSITIONS ARE SKIPPED (change 0070 D5), which is the enforcement point
// of this change's invariant. An opaque arg's stored text is the raw source of
// the word ("$VAR", "$(echo /)"), not a path, and every caller of this function
// hands its result to withinWorkspace — which resolves a relative operand
// against the cwd and walks up to the deepest EXISTING ancestor. Emitting
// "$(echo /)" would therefore resolve it to <cwd>/$(echo /), whose parent IS the
// workspace, and PROVE containment for a command that deletes the filesystem
// root. Same failure family as #0068's unexpanded `~` and process-name
// operands: a containment proof is sound only over an operand that is a path.
//
// It iterates by INDEX rather than `range seg.Args` by value precisely so the
// opaqueness flag — which lives in a parallel slice — is still reachable.
//
// Skipping here is only half the answer: dropping an operand also removes the
// thing the caller would have failed on. classifyHeuristic supplies the other
// half by asking outright on any mutating segment with an opaque arg. Neither
// guard is safe without the other.
func pathArgs(seg Segment) []string {
	var out []string
	for i, a := range seg.Args {
		if a == "" {
			continue
		}
		if seg.isOpaque(i) {
			continue // unprovable ⇒ never resolved as a path. See above.
		}
		if strings.HasPrefix(a, "-") {
			continue // an option flag, not a path operand.
		}
		// dd's operands are key=value (of=/tmp/x, if=in.img): scope the value's
		// path, not the literal "of=/tmp/x" token — which would resolve as a
		// weird cwd-relative filename and wrongly prove containment (0068).
		if seg.Name == "dd" {
			if v, ok := strings.CutPrefix(a, "of="); ok {
				out = append(out, v)
				continue
			}
			if v, ok := strings.CutPrefix(a, "if="); ok {
				out = append(out, v)
				continue
			}
		}
		out = append(out, a)
	}
	return out
}

// withinWorkspace reports whether arg, resolved symlink-aware, stays within
// workspaceRoot. It resolves the deepest existing ancestor when the leaf does
// not yet exist (a file about to be created), so a to-be-written path scopes on
// its real parent directory rather than a lexical guess.
//
// This never trusts a lexical prefix of the pre-resolution path: an in-workspace
// symlink whose target escapes the root is caught because the link itself is
// resolved (dirent-isdir-skips-symlinks). A relative arg resolves against the
// current working directory (the caller's process cwd == the workspace).
func withinWorkspace(arg, workspaceRoot string) bool {
	// Tilde reaches us as a literal (the parser does not expand it) but bash
	// WILL expand it at execution: ~/x is a home-anchored path, not a cwd file
	// named "~". Expand our own home; a ~user form for another user is
	// unprovable ⇒ fail toward the human (change 0068).
	if arg == "~" || strings.HasPrefix(arg, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return false
		}
		arg = home + arg[1:]
	} else if strings.HasPrefix(arg, "~") {
		return false
	}
	abs, err := filepath.Abs(arg)
	if err != nil {
		return false // cannot resolve ⇒ fail toward the human.
	}

	resolved, ok := resolveExisting(abs)
	if !ok {
		return false
	}
	return isWithin(resolved, workspaceRoot)
}

// resolveExisting resolves p with EvalSymlinks. When p's leaf does not exist
// yet, it resolves the deepest existing ancestor and re-appends the missing
// trailing components, so a not-yet-created leaf still scopes on its real
// parent. ok is false if not even an ancestor can be resolved.
func resolveExisting(p string) (string, bool) {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved, true
	}
	// The leaf (or deeper) does not exist. Walk up to the deepest ancestor that
	// does, resolve it, then re-append the missing tail.
	dir := p
	var missing []string
	for {
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false // reached the root without finding an existing ancestor.
		}
		missing = append([]string{filepath.Base(dir)}, missing...)
		dir = parent
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(append([]string{resolved}, missing...)...), true
		}
	}
}

// isWithin reports whether path is workspaceRoot itself or a descendant of it,
// using a cleaned, separator-boundary-aware prefix on the resolved paths (both
// already symlink-resolved by the caller).
func isWithin(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	// Any rel that steps up out of root begins with "..".
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}
