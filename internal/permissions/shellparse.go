package permissions

import (
	"errors"
	"path"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// ErrUnparseable is the sentinel returned by splitSegments when a command
// cannot be safely decomposed into simple-command segments. The caller must
// fail closed: treat the whole command as a single opaque, non-approvable unit
// (prompt in interactive mode, deny in auto mode's non-interactive posture).
var ErrUnparseable = errors.New("permissions: command is unparseable")

// maxCommandBytes caps the input length. Oversized commands fail closed rather
// than feed an unbounded parse. Roughly Grok Build's ~10k character cap.
const maxCommandBytes = 10000

// Segment is one simple command extracted from a (possibly compound) shell
// command. Name is the basename of argv[0]; Args are the remaining words in
// order; Raw is the original source text of the segment.
type Segment struct {
	Name string
	Args []string
	Raw  string
}

// arbitraryArgWrappers are argv[0] names that run an arbitrary inner command
// supplied as data. They are never peeled and never inherit the inner
// command's allow decision, so their presence as a bare command fails closed.
// bash/sh are handled specially (a parseable `-c "<script>"` is peeled; any
// other form fails closed).
var arbitraryArgWrappers = map[string]bool{
	"xargs":  true,
	"env":    true,
	"npx":    true,
	"sudo":   true,
	"watch":  true,
	"nohup":  true,
	"docker": true,
	"eval":   true,
	"exec":   true,
	// timeout takes a duration then an arbitrary inner command. The read-only
	// safe-list classifier (isReadOnlySafe in rules.go) now exists, so a future
	// task MAY peel timeout — but that requires stripping timeout's own flags
	// AND its mandatory duration argument here in shellparse.go, plus new corpus
	// rows. Kept fail-closed for now ("timeout-then-unknown"): the conservative
	// posture costs a human prompt, never a bypass.
	"timeout": true,
}

// peelWrappers are side-effect-free prefixes stripped before the argv[0]
// check. Their first non-flag argument becomes the effective command.
var peelWrappers = map[string]bool{
	"time":   true,
	"nice":   true,
	"stdbuf": true,
}

// splitSegments parses cmd as a bash program and enumerates every
// simple-command segment across &&, ||, ;, |, |&, and newlines, including the
// script bodies of `bash -c "…"` / `sh -c "…"`.
//
// It fails closed with ErrUnparseable on: a size-cap violation; a parse error;
// command substitution ($(…) or backticks) or process substitution anywhere;
// any redirection whose file target the read-only classifier never sees —
// EXCEPT the two benign shapes a redirect to literal /dev/null and a pure
// fd-duplication (2>&1, >&2), which carry no writable target (see
// redirIsBenign, change 0037); an argv[0] containing a path separator (the
// basename-collapse bug); or an arbitrary-arg wrapper as argv[0] (bash/sh
// without a parseable -c, xargs, env with assignments, npx,
// timeout-then-unknown, sudo, …).
func splitSegments(cmd string) ([]Segment, error) {
	if len(cmd) > maxCommandBytes {
		return nil, ErrUnparseable
	}
	parser := syntax.NewParser(syntax.Variant(syntax.LangBash))
	file, err := parser.Parse(strings.NewReader(cmd), "")
	if err != nil {
		return nil, ErrUnparseable
	}

	var segs []Segment
	if err := collectStmts(cmd, file.Stmts, &segs); err != nil {
		return nil, err
	}
	return segs, nil
}

// collectStmts walks a slice of statements, appending a Segment for each
// simple command it reaches (recursing into peelable/`-c` wrappers).
func collectStmts(src string, stmts []*syntax.Stmt, out *[]Segment) error {
	for _, st := range stmts {
		if err := collectStmt(src, st, out); err != nil {
			return err
		}
	}
	return nil
}

func collectStmt(src string, st *syntax.Stmt, out *[]Segment) error {
	if st == nil || st.Cmd == nil {
		return nil
	}
	// A redirect (>, >>, 2>, <, here-doc, …) points a command's fd at a file
	// whose target the read-only classifier never sees: collectCall only
	// inspects CallExpr.Args, so `echo x > /etc/passwd` would classify as the
	// read-only `echo` and reach VerdictAllow. We therefore fail closed on any
	// redirect EXCEPT the two benign shapes (change 0037): a redirect to the
	// literal /dev/null sink and a pure fd-duplication (2>&1, >&2). Those two
	// carry no writable file target the classifier could miss, so silencing
	// stderr on a read-only pipeline (`wc -l x.go 2>/dev/null | tail`) no longer
	// stalls. Any redirect naming a real path, a variable, or a substitution —
	// or a here-doc / <> — still fails closed. The conservative posture costs a
	// human prompt, never a silent bypass.
	for _, r := range st.Redirs {
		if !redirIsBenign(r) {
			return ErrUnparseable
		}
	}
	switch cmd := st.Cmd.(type) {
	case *syntax.BinaryCmd:
		// &&, ||, | and |& all decompose into their operands.
		if err := collectStmt(src, cmd.X, out); err != nil {
			return err
		}
		return collectStmt(src, cmd.Y, out)
	case *syntax.CallExpr:
		return collectCall(src, cmd, out)
	default:
		// Control-flow constructs, subshells, blocks, function decls, etc.
		// are not simple commands we can decompose safely: fail closed.
		return ErrUnparseable
	}
}

// redirIsBenign reports whether a single redirect is safe to allow past the
// fail-closed guard (change 0037). Exactly two shapes qualify:
//
//  1. A redirect to the literal /dev/null sink — op >, >>, <, &>, or &>> with a
//     word that resolves to the exact literal "/dev/null". A here-doc body
//     (r.Hdoc != nil) never qualifies, and the literal check (via literalWord)
//     rejects any target carrying a variable, glob, or substitution — so
//     "/dev/null.txt", "/dev/nul", "$F", and "$(…)" all fail closed.
//  2. A pure fd-duplication — op <& or >& (DplIn/DplOut) whose word is a bare
//     numeric file descriptor (the "1" in 2>&1, the "2" in >&2). No file path
//     is named, so there is nothing the classifier could miss.
//
// Everything else — a real file target, <> (RdrInOut), a here-doc, a dup to a
// path — returns false, and the statement fails closed.
func redirIsBenign(r *syntax.Redirect) bool {
	if r == nil {
		return false
	}
	// A here-document is never benign regardless of op.
	if r.Hdoc != nil {
		return false
	}
	switch r.Op {
	case syntax.RdrOut, syntax.AppOut, syntax.RdrIn, syntax.RdrAll, syntax.AppAll:
		// File-target ops: benign only when the literal target is /dev/null.
		if r.Word == nil {
			return false
		}
		target, ok := literalWord(r.Word)
		return ok && target == "/dev/null"
	case syntax.DplIn, syntax.DplOut:
		// fd-duplication: benign only when the target is a bare fd number.
		if r.Word == nil {
			return false
		}
		target, ok := literalWord(r.Word)
		return ok && isAllDigits(target)
	default:
		// RdrInOut (<>), here-doc ops, and anything else: fail closed.
		return false
	}
}

// isAllDigits reports whether s is non-empty and every rune is an ASCII digit.
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// collectCall turns a simple command into one Segment, or recurses into a
// `-c` script body, or fails closed on a wrapper / substitution / path-
// qualified argv[0].
func collectCall(src string, call *syntax.CallExpr, out *[]Segment) error {
	// Env-var assignment prefixes (FOO=bar cmd) fail closed at the parse
	// floor; higher layers decide whether the stripped form is allowable.
	if len(call.Assigns) > 0 {
		return ErrUnparseable
	}
	if len(call.Args) == 0 {
		// Assignment-only statement with no command (already handled above)
		// or an empty call; nothing to enumerate.
		return nil
	}

	// Extract every word as a plain literal, failing closed on any expansion
	// we cannot statically resolve (command/process substitution).
	words := make([]string, 0, len(call.Args))
	for _, w := range call.Args {
		lit, ok := literalWord(w)
		if !ok {
			return ErrUnparseable
		}
		words = append(words, lit)
	}

	argv0 := words[0]

	// Peel side-effect-free wrappers (timeout/time/nice/stdbuf); the peeled
	// remainder must itself be a bare, non-path-qualified command.
	for peelWrappers[basename(argv0)] && !strings.ContainsRune(argv0, '/') {
		words = peelWrapperArgs(words)
		if len(words) == 0 {
			return ErrUnparseable
		}
		argv0 = words[0]
	}

	// Path-qualified argv[0] (./sed, /tmp/git) fails closed: basename
	// matching must never collapse a path to a bare name.
	if strings.ContainsRune(argv0, '/') {
		return ErrUnparseable
	}

	name := basename(argv0)

	// bash/sh: only a parseable `-c "<script>"` form is peeled into its inner
	// segments; every other form (bare, a script file, no script word) fails
	// closed.
	if name == "bash" || name == "sh" {
		script, ok := dashCScript(words[1:])
		if !ok {
			return ErrUnparseable
		}
		inner := syntax.NewParser(syntax.Variant(syntax.LangBash))
		file, err := inner.Parse(strings.NewReader(script), "")
		if err != nil {
			return ErrUnparseable
		}
		return collectStmts(script, file.Stmts, out)
	}

	// Any other arbitrary-arg wrapper as argv[0] fails closed.
	if arbitraryArgWrappers[name] {
		return ErrUnparseable
	}

	seg := Segment{
		Name: name,
		Args: append([]string(nil), words[1:]...),
		Raw:  rawSegment(src, call),
	}
	if len(seg.Args) == 0 {
		seg.Args = nil
	}
	*out = append(*out, seg)
	return nil
}

// literalWord returns the fully-literal string value of a word, or ok=false if
// the word contains a command or process substitution (unresolvable
// statically). Parameter expansions and quoting resolve to their literal text
// where possible; a bare $VAR yields ok=false so command bodies like
// `curl $URL` fail closed rather than silently drop the argument.
func literalWord(w *syntax.Word) (string, bool) {
	var b strings.Builder
	for _, part := range w.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			b.WriteString(p.Value)
		case *syntax.SglQuoted:
			b.WriteString(p.Value)
		case *syntax.DblQuoted:
			inner := &syntax.Word{Parts: p.Parts}
			s, ok := literalWord(inner)
			if !ok {
				return "", false
			}
			b.WriteString(s)
		case *syntax.CmdSubst, *syntax.ProcSubst, *syntax.ParamExp, *syntax.ArithmExp, *syntax.ExtGlob:
			return "", false
		default:
			return "", false
		}
	}
	return b.String(), true
}

// dashCScript returns the script argument of a `-c "<script>"` invocation.
// args are the words after argv[0]. It accepts `-c script [name [arg...]]` and
// requires a script word to be present.
func dashCScript(args []string) (string, bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-c" {
			if i+1 >= len(args) {
				return "", false
			}
			return args[i+1], true
		}
		// A combined `-c<script>` form.
		if strings.HasPrefix(a, "-c") && len(a) > 2 {
			return a[2:], true
		}
		// Any other leading flag/word before -c means this is not a plain
		// `-c` invocation we can safely peel.
		return "", false
	}
	return "", false
}

// peelWrapperArgs drops the wrapper argv[0] and any of its own leading flags,
// returning the inner command's words.
func peelWrapperArgs(words []string) []string {
	rest := words[1:]
	for len(rest) > 0 && strings.HasPrefix(rest[0], "-") {
		rest = rest[1:]
	}
	return rest
}

// basename returns the final path element of argv0. For a bare name (no
// separator) this is the name unchanged.
func basename(argv0 string) string {
	return path.Base(argv0)
}

// rawSegment returns the original source text spanning the call expression.
func rawSegment(src string, call *syntax.CallExpr) string {
	start := call.Pos().Offset()
	end := call.End().Offset()
	if int(end) > len(src) {
		end = uint(len(src))
	}
	if start > end {
		return ""
	}
	return strings.TrimSpace(src[start:end])
}
