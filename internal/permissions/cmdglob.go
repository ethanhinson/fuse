package permissions

import (
	"path"
	"unicode/utf8"
)

// matchProtective reports whether subject matches pat under the generous glob
// that PROTECTIVE consumers (auto.deny, auto.ask, always_prompt) use (change
// 0088). It is path.Match with one difference: '*' matches any run of
// characters INCLUDING '/', and '?' matches any one character including '/'.
// Character classes ('[...]', '[^...]', ranges) and '\' escapes follow
// path.Match exactly.
//
// Why two engines: path.Match's '*' stops at '/', which is the containment an
// ALLOW pattern like "bash:rm build/*" relies on (ADR-0005, ADR-0049), so the
// permissive consumer keeps it. For a deny or ask rule that same boundary is a
// fail-open: "bash:rm *" missed "rm -rf a/b". A protective match only ever
// tightens, so matching more text is the safe direction.
//
// A malformed pattern matches nothing, which is how the old
// `ok, _ := path.Match(...)` call sites already treated ErrBadPattern.
func matchProtective(pat, subject string) bool {
	// path.Match validates the whole pattern even when it does not match, so
	// probing against "" is a complete syntax check.
	if _, err := path.Match(pat, ""); err != nil {
		return false
	}
	return globSpanSlash(pat, subject)
}

// globSpanSlash matches a pre-validated pattern. It is the classic
// single-star-backtrack wildcard loop: on a mismatch it resumes at the most
// recent '*', letting it swallow one more byte. Literal and escaped bytes are
// compared byte for byte and '*' advances one byte at a time, mirroring
// path.Match; '?' and classes consume one rune, also mirroring path.Match.
func globSpanSlash(pat, s string) bool {
	px, sx := 0, 0
	starPx, starSx := -1, 0
	for px < len(pat) || sx < len(s) {
		if px < len(pat) {
			switch pat[px] {
			case '*':
				starPx, starSx = px, sx
				px++
				continue
			case '?':
				if sx < len(s) {
					_, n := utf8.DecodeRuneInString(s[sx:])
					px++
					sx += n
					continue
				}
			case '[':
				if sx < len(s) {
					r, n := utf8.DecodeRuneInString(s[sx:])
					if ok, end := matchGlobClass(pat, px, r); ok {
						px = end
						sx += n
						continue
					}
				}
			case '\\':
				// Validation guarantees an escape is followed by a byte.
				if sx < len(s) && pat[px+1] == s[sx] {
					px += 2
					sx++
					continue
				}
			default:
				if sx < len(s) && pat[px] == s[sx] {
					px++
					sx++
					continue
				}
			}
		}
		if starPx >= 0 && starSx < len(s) {
			starSx++
			px, sx = starPx+1, starSx
			continue
		}
		return false
	}
	return true
}

// matchGlobClass reports whether r matches the character class that starts at
// pat[px] == '[', and returns the index just past its closing ']'. The pattern
// is pre-validated, so the class is well formed and non-empty.
func matchGlobClass(pat string, px int, r rune) (bool, int) {
	i := px + 1
	negated := false
	if i < len(pat) && pat[i] == '^' {
		negated = true
		i++
	}
	matched := false
	for n := 0; ; n++ {
		if n > 0 && pat[i] == ']' {
			i++
			break
		}
		lo, w := globClassChar(pat, i)
		i += w
		hi := lo
		if pat[i] == '-' {
			h, w2 := globClassChar(pat, i+1)
			hi = h
			i += 1 + w2
		}
		if lo <= r && r <= hi {
			matched = true
		}
	}
	return matched != negated, i
}

// globClassChar decodes one (possibly '\'-escaped) class character at pat[i]
// and returns it with its encoded width.
func globClassChar(pat string, i int) (rune, int) {
	if pat[i] == '\\' {
		r, n := utf8.DecodeRuneInString(pat[i+1:])
		return r, 1 + n
	}
	return utf8.DecodeRuneInString(pat[i:])
}
