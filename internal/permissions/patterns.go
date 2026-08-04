package permissions

import (
	"encoding/json"
	"path"
	"strings"
)

// matchesAny returns true if name (or "name:firstToken" for bash) matches any
// glob in patterns. Uses path.Match semantics (same as filepath.Match but
// always uses '/' as separator for cross-platform consistency).
func matchesAny(patterns []string, toolName, args string) bool {
	subjects := subjects(toolName, args)
	for _, pat := range patterns {
		for _, subj := range subjects {
			if ok, _ := path.Match(pat, subj); ok {
				return true
			}
		}
	}
	return false
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
