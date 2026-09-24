package skills

import (
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
)

// Activator evaluates the declared activation triggers of a skill set against
// what a run observably does: the request text at the start, and the paths
// each tool call touches after that. It is deterministic, makes no model
// call, and attaches each skill at most once per run. It never decides for
// the model what to do with the body; it only makes the body present.
type Activator struct {
	skills   []Skill
	attached map[string]bool
}

// NewActivator returns an activator over the skills that declare triggers.
// A set with no declared triggers yields a nil activator, which every method
// treats as "nothing to do".
func NewActivator(set *Set) *Activator {
	if set == nil {
		return nil
	}
	var withTriggers []Skill
	for _, sk := range set.All() {
		if sk.Activation.Declared() {
			withTriggers = append(withTriggers, sk)
		}
	}
	if len(withTriggers) == 0 {
		return nil
	}
	return &Activator{skills: withTriggers, attached: map[string]bool{}}
}

// Match is one fired trigger: the skill and the reason it fired, in words a
// transcript reader (or the model) can check.
type Match struct {
	Skill  Skill
	Reason string
}

// OnRequest fires `always` skills and phrase / path triggers found in the
// request text. Called once, with the user's initial request.
func (a *Activator) OnRequest(request string) []Match {
	if a == nil {
		return nil
	}
	lower := strings.ToLower(request)
	var out []Match
	for _, sk := range a.skills {
		if a.attached[sk.Name] {
			continue
		}
		switch {
		case sk.Activation.Always:
			out = a.fire(out, sk, "activation.always")
		case phraseIn(lower, sk.Activation.Phrases) != "":
			out = a.fire(out, sk, fmt.Sprintf("the request mentions %q", phraseIn(lower, sk.Activation.Phrases)))
		case pathIn(mentionedPaths(request), sk.Activation.Paths) != "":
			out = a.fire(out, sk, fmt.Sprintf("the request names %s", pathIn(mentionedPaths(request), sk.Activation.Paths)))
		}
	}
	return out
}

// OnToolCall fires path triggers against the path arguments of one tool call
// (read/write/edit/list/grep `path`, and any path-looking token of a bash
// `command`). Called at the turn boundary after the call executed.
func (a *Activator) OnToolCall(name, arguments string) []Match {
	if a == nil {
		return nil
	}
	paths := callPaths(name, arguments)
	if len(paths) == 0 {
		return nil
	}
	var out []Match
	for _, sk := range a.skills {
		if a.attached[sk.Name] || len(sk.Activation.Paths) == 0 {
			continue
		}
		if p := pathIn(paths, sk.Activation.Paths); p != "" {
			out = a.fire(out, sk, fmt.Sprintf("%s touched %s", name, p))
		}
	}
	return out
}

func (a *Activator) fire(out []Match, sk Skill, reason string) []Match {
	a.attached[sk.Name] = true
	return append(out, Match{Skill: sk, Reason: reason})
}

func phraseIn(lowerText string, phrases []string) string {
	for _, ph := range phrases {
		if ph != "" && strings.Contains(lowerText, strings.ToLower(ph)) {
			return ph
		}
	}
	return ""
}

// pathIn returns the first candidate matching any glob. Globs use path.Match
// syntax; a leading "**/" matches any directory depth (so "**/*.py" matches
// "a/b/c.py" and "c.py"); a trailing "/**" matches anything under a directory.
func pathIn(candidates, globs []string) string {
	for _, c := range candidates {
		c = path.Clean(strings.ReplaceAll(c, "\\", "/"))
		c = strings.TrimPrefix(c, "./")
		for _, g := range globs {
			if globMatch(g, c) {
				return c
			}
		}
	}
	return ""
}

func globMatch(glob, p string) bool {
	if strings.HasSuffix(glob, "/**") {
		dir := strings.TrimSuffix(glob, "/**")
		return p == dir || strings.HasPrefix(p, dir+"/") || strings.Contains(p, "/"+dir+"/") || strings.HasPrefix(p, "/"+dir+"/")
	}
	if strings.HasPrefix(glob, "**/") {
		tail := strings.TrimPrefix(glob, "**/")
		if ok, _ := path.Match(tail, p); ok {
			return true
		}
		if ok, _ := path.Match(tail, path.Base(p)); ok {
			return true
		}
		// any suffix of the path, so "**/x/*.py" matches "a/x/b.py"
		parts := strings.Split(p, "/")
		for i := range parts {
			if ok, _ := path.Match(tail, strings.Join(parts[i:], "/")); ok {
				return true
			}
		}
		return false
	}
	ok, _ := path.Match(glob, p)
	if !ok {
		ok, _ = path.Match(glob, path.Base(p))
	}
	return ok
}

var pathToken = regexp.MustCompile(`(?:^|[\s"'=(\[,])((?:\.{1,2}/|/|[A-Za-z0-9_.-]+/)?[A-Za-z0-9_./-]*[A-Za-z0-9_-]\.[A-Za-z0-9]{1,8}|\.[A-Za-z0-9_-]+/[A-Za-z0-9_./-]*|[A-Za-z0-9_.-]+/[A-Za-z0-9_./-]+)`)

// mentionedPaths pulls path-looking tokens (something with a slash or a file
// extension) out of free text.
func mentionedPaths(text string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range pathToken.FindAllStringSubmatch(text, -1) {
		t := strings.TrimRight(m[1], ".,;:)")
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// callPaths extracts the paths a tool call touches from its JSON arguments.
func callPaths(name, arguments string) []string {
	var args map[string]any
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return nil
	}
	var out []string
	for _, key := range []string{"path", "file_path", "working_dir"} {
		if v, ok := args[key].(string); ok && v != "" {
			out = append(out, v)
		}
	}
	if name == "bash" {
		if cmd, ok := args["command"].(string); ok {
			out = append(out, mentionedPaths(cmd)...)
		}
	}
	return out
}
