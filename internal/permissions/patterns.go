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
// "bash:<firstToken>"} match is kept for compatibility, so nothing that
// allowed before stops allowing. It additionally admits a bash command when
// EVERY parsed segment matches strictly, through the same allSegmentsAllowed
// auto mode uses (path.Match, opaque-arg and redirect guards, no allow_push),
// so "bash:git *" means the same thing in both modes and never crosses '/'.
// That segment path declines when any segment is catastrophic
// (isDangerous against workspaceRoot): auto mode denies those before any
// allow is consulted, and smart mode has no deny consumer, so without this
// floor "bash:rm *" would silently approve `rm -rf ~`. The call asks instead.
func matchesPermissive(patterns []string, toolName, args, workspaceRoot string) bool {
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
	for _, seg := range segments {
		if isDangerous(seg, workspaceRoot) {
			return false
		}
	}
	return allSegmentsAllowed(segments, patterns, false)
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
