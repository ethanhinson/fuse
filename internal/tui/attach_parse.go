package tui

import (
	"net/url"
	"path/filepath"
	"strings"
)

const (
	// maxDropTokens is the most paths one paste may hold and still count as a drop.
	maxDropTokens = 16
	// maxDropBytes is the longest paste considered as a drop.
	maxDropBytes = 8192
)

func isASCIISpace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\r' || r == '\n'
}

// parseDroppedPaths is a minimal shell-word splitter covering what terminals
// paste when a file is dropped on them: backslash-escaped spaces, single or
// double quotes, and several paths separated by spaces or newlines. Only ASCII
// whitespace separates tokens, because macOS screenshot names hold U+202F and
// terminals do not escape it. ok is false for an unterminated quote or a
// trailing lone backslash.
func parseDroppedPaths(s, goos string) ([]string, bool) {
	windows := goos == "windows"
	rs := []rune(strings.Trim(s, " \t\r\n"))
	var tokens []string
	var cur []rune
	inTok := false
	flush := func() {
		if inTok {
			tokens = append(tokens, string(cur))
			cur, inTok = nil, false
		}
	}
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case isASCIISpace(r):
			flush()
		case r == '\'':
			inTok = true
			j := i + 1
			for j < len(rs) && rs[j] != '\'' {
				cur = append(cur, rs[j])
				j++
			}
			if j >= len(rs) {
				return nil, false
			}
			i = j
		case r == '"':
			inTok = true
			j := i + 1
			closed := false
			for ; j < len(rs); j++ {
				c := rs[j]
				if c == '"' {
					closed = true
					break
				}
				if c == '\\' && !windows && j+1 < len(rs) {
					switch rs[j+1] {
					case '"', '\\', '$', '`':
						cur = append(cur, rs[j+1])
						j++
						continue
					}
				}
				cur = append(cur, c)
			}
			if !closed {
				return nil, false
			}
			i = j
		case r == '\\' && !windows:
			if i+1 >= len(rs) {
				return nil, false
			}
			inTok = true
			cur = append(cur, rs[i+1])
			i++
		default:
			inTok = true
			cur = append(cur, r)
		}
	}
	flush()
	return tokens, true
}

// resolveDroppedPath turns one token into an absolute, cleaned path: file: URLs
// are decoded, a leading ~ is expanded, and anything not absolute is refused.
func resolveDroppedPath(tok, home, goos string) (string, bool) {
	p, ok := expandToken(tok, home, goos)
	if !ok {
		return "", false
	}
	p = filepath.Clean(p)
	if !filepath.IsAbs(p) {
		return "", false
	}
	return p, true
}

// expandToken handles file: URLs and ~ but does not require an absolute path.
func expandToken(tok, home, goos string) (string, bool) {
	if strings.HasPrefix(tok, "file:") {
		u, err := url.Parse(tok)
		if err != nil || (u.Host != "" && u.Host != "localhost") {
			return "", false
		}
		p := u.Path
		if goos == "windows" && len(p) >= 3 && p[0] == '/' && p[2] == ':' {
			p = p[1:]
		}
		return p, p != ""
	}
	if tok == "~" || strings.HasPrefix(tok, "~/") {
		if home == "" {
			return "", false
		}
		return home + tok[1:], true
	}
	if strings.HasPrefix(tok, "~") {
		return "", false
	}
	return tok, tok != ""
}

// parseDrop tokenizes s and resolves every token. It is false if any token is
// not an absolute path.
func parseDrop(s, home, goos string) ([]string, bool) {
	toks, ok := parseDroppedPaths(s, goos)
	if !ok || len(toks) == 0 {
		return nil, false
	}
	out := make([]string, len(toks))
	for i, t := range toks {
		p, ok := resolveDroppedPath(t, home, goos)
		if !ok {
			return nil, false
		}
		out[i] = p
	}
	return out, true
}

// parseAttachArgs is parseDrop for /attach: a relative token is joined to cwd.
func parseAttachArgs(s, home, cwd, goos string) ([]string, bool) {
	toks, ok := parseDroppedPaths(s, goos)
	if !ok || len(toks) == 0 {
		return nil, false
	}
	out := make([]string, len(toks))
	for i, t := range toks {
		p, ok := expandToken(t, home, goos)
		if !ok {
			return nil, false
		}
		if !filepath.IsAbs(p) {
			if cwd == "" {
				return nil, false
			}
			p = filepath.Join(cwd, p)
		}
		out[i] = filepath.Clean(p)
	}
	return out, true
}
