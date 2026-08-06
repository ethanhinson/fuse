package permissions

import (
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
// workspaceRoot must already be canonicalized (filepath.EvalSymlinks) by the
// caller so containment comparisons are made against the real path.
func classifyHeuristic(segments []Segment, workspaceRoot string) Verdict {
	if len(segments) == 0 {
		return VerdictAsk
	}

	// 1. Egress boundary wins first — never a silent allow.
	for _, seg := range segments {
		if isEgress(seg) {
			return VerdictAsk
		}
	}

	// 2. Wholly read-only ⇒ allow, no scoping needed.
	if allSegmentsReadOnlySafe(segments) {
		return VerdictAllow
	}

	// 3. Mutating: every path argument must stay inside the workspace.
	for _, seg := range segments {
		if isReadOnlySafe(seg) {
			continue // a read-only segment mutates nothing to scope.
		}
		for _, arg := range pathArgs(seg) {
			if !withinWorkspace(arg, workspaceRoot) {
				return VerdictAsk
			}
		}
	}
	return VerdictAllow
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
// tool, or a git remote operation given a host-qualified URL.
func isEgress(seg Segment) bool {
	if egressNames[seg.Name] {
		return true
	}
	if seg.Name == "git" && len(seg.Args) > 0 && gitEgressSubcommands[seg.Args[0]] {
		for _, a := range seg.Args[1:] {
			if isHostQualified(a) {
				return true
			}
		}
	}
	return false
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
func pathArgs(seg Segment) []string {
	var out []string
	for _, a := range seg.Args {
		if a == "" {
			continue
		}
		if strings.HasPrefix(a, "-") {
			continue // an option flag, not a path operand.
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
