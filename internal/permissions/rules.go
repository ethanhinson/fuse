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
