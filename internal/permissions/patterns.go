package permissions

import (
	"encoding/json"
	"strings"
)

// protectivePattern returns the first protective pattern (always_prompt in
// smart mode) that matches a tool call, and whether one did (changes 0088,
// 0092). Subject order is unchanged: the legacy {tool, "bash:<firstToken>"}
// subjects first, so no rule that demoted a call before stops demoting it,
// then the one shared protective subject rule (protectiveHitPattern: each
// parsed segment, its head, its raw pre-peel text, and the whole command) with
// the slash-spanning glob. A command the parser refuses is matched on its raw
// text instead, so a protective rule still reaches it. Returning the pattern
// lets a smart-mode deny name the rule exactly as the auto-mode reason does.
func protectivePattern(patterns []string, toolName, args string) (string, bool) {
	if len(patterns) == 0 {
		return "", false
	}
	for _, subj := range subjects(toolName, args) {
		if pat, ok := firstProtectiveMatch(patterns, subj); ok {
			return pat, true
		}
	}
	if toolName != "bash" {
		return "", false
	}
	command := bashCommand(args)
	segments, err := splitSegments(command)
	if err != nil {
		raw := strings.TrimSpace(command)
		if raw == "" {
			return "", false
		}
		return firstProtectiveMatch(patterns, "bash:"+raw)
	}
	return protectiveHitPattern(patterns, segments)
}

// matchesPermissive reports whether an allow pattern (auto_approve in smart
// mode) admits a tool call (change 0088). The legacy {tool,
// "bash:<firstToken>"} match is kept for compatibility, and it additionally
// admits a bash command when EVERY parsed segment matches strictly, through
// the same allSegmentsAllowed auto mode uses (path.Match, opaque-arg and
// redirect guards, no allow_push), so "bash:git *" means the same thing in
// both modes and never crosses '/'.
//
// Since change 0093, no allow (legacy first-token or per-segment) admits a
// command with a catastrophic segment (catastrophicSegment, isDangerous
// against workspaceRoot): auto mode denies those before any allow is
// consulted, and without this floor "bash:rm" would silently approve
// `rm -rf ~`. A command the parser refuses keeps the legacy match: there is
// no segment to judge, and "bash:sudo" in auto_approve is the operator's
// explicit choice.
//
// The floor alone does not close the legacy allow (change 0093): isDangerous
// skips an opaque rm operand, because it cannot prove `rm -rf $HOME` is
// catastrophic. Auto mode never approves that shape (allSegmentsAllowed
// declines any opaque arg and the scoping layer asks), so a parsed command with
// an unprovableDestructive segment declines the legacy allow too and reaches
// the human. The guard is narrower than allSegmentsAllowed's: an opaque
// operand on a command that cannot destroy a machine (`git commit -m "$MSG"`
// under "bash:git") keeps its legacy allow.
func matchesPermissive(patterns []string, toolName, args, workspaceRoot string) bool {
	if len(patterns) == 0 {
		return false
	}
	if _, catastrophic := catastrophicSegment(toolName, args, workspaceRoot); catastrophic {
		return false
	}
	var segments []Segment
	parsed := false
	if toolName == "bash" {
		var err error
		segments, err = splitSegments(bashCommand(args))
		parsed = err == nil
	}
	if parsed {
		for _, seg := range segments {
			if unprovableDestructive(seg) {
				return false
			}
		}
	}
	for _, subj := range subjects(toolName, args) {
		if matchesSubjects(consumerPermissive, patterns, subj) {
			return true
		}
	}
	if !parsed || len(segments) == 0 {
		return false
	}
	return allSegmentsAllowed(segments, patterns, false)
}

// unprovableDestructive reports whether a segment is a catastrophe-capable
// command (rm or dd, the two shape-based cases isDangerous judges by operand)
// carrying an opaque argument (change 0093). isCatastrophicRm skips an opaque
// word rather than resolve it, and isDdToDevice only sees a literal of=/dev/
// prefix, so `rm -rf $HOME`, `rm -rf $'\x2f'` and `dd of=$DEV` are not
// provably catastrophic; they are not provably safe either. Any opaque rm word
// counts, not only an operand: an unquoted `$ARGS` can split into `-rf /`. The
// name-only catastrophic commands (mkfs, mkfs.*, shutdown and the rest of
// dangerousNames) are floored by isDangerous whatever their arguments.
func unprovableDestructive(seg Segment) bool {
	if seg.Name != "rm" && seg.Name != "dd" {
		return false
	}
	return seg.hasOpaqueArg()
}

// catastrophicSegment reports whether a bash call parses into at least one
// catastrophic segment (isDangerous against workspaceRoot) and returns the
// command text for the decision reason (change 0093). It is the one floor
// check smart mode's allow step (matchesPermissive) and the smart branch of
// resolve share, so the two cannot disagree. A non-bash tool, or a command the
// parser refuses (sudo, npx, xargs, a path-qualified argv0, an opaque command
// word), is never catastrophic here: there is no segment to judge.
func catastrophicSegment(toolName, args, workspaceRoot string) (string, bool) {
	if toolName != "bash" {
		return "", false
	}
	command := bashCommand(args)
	segments, err := splitSegments(command)
	if err != nil {
		return "", false
	}
	for _, seg := range segments {
		if isDangerous(seg, workspaceRoot) {
			return command, true
		}
	}
	return "", false
}

// subjects returns the match candidates for a tool call. For bash it adds
// "bash:<firstToken>" so patterns like "git push" work.
func subjects(toolName, args string) []string {
	out := []string{toolName}
	if toolName == "bash" {
		if first := firstToken(args); first != "" {
			out = append(out, "bash:"+first)
		}
	}
	return out
}

// firstToken extracts the first whitespace-separated token from the bash
// command string in args JSON ({"command":"..."}).
func firstToken(args string) string {
	var v struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal([]byte(args), &v); err != nil {
		return ""
	}
	fields := strings.Fields(v.Command)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}
