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

// dangerousEnvVars are environment-variable names that change *what code runs*
// rather than merely how it behaves: dynamic-loader hooks, command lookup, word
// splitting, shell startup files, and the "run this helper for me" hooks of git,
// ssh, python, node, perl and ruby. An assignment prefix naming one of these can
// turn an otherwise-innocent command into arbitrary code execution
// (`LD_PRELOAD=evil.so make`), which the argv-based classifier downstream can
// never see. Names here fail the whole command closed (change 0070 D1).
//
// Keys are upper-cased; lookup goes through dangerousEnvVarName.
var dangerousEnvVars = map[string]bool{
	"LD_PRELOAD":      true,
	"LD_LIBRARY_PATH": true,
	"LD_AUDIT":        true,
	"PATH":            true,
	"IFS":             true,
	"BASH_ENV":        true,
	"ENV":             true,
	"SHELL":           true,
	"PS4":             true,
	"PROMPT_COMMAND":  true,
	"GIT_SSH_COMMAND": true,
	"GIT_ASKPASS":     true,
	"SSH_ASKPASS":     true,
	"PYTHONSTARTUP":   true,
	"NODE_OPTIONS":    true,
	"PERL5LIB":        true,
	"RUBYOPT":         true,

	// Pager/editor hooks: names whose value a tool will happily exec. git
	// consults PAGER for `log`/`diff`, and LESSOPEN is an arbitrary input
	// filter program. `git log` and `git diff` are on the auto-approve
	// safelist (readOnlyUtils/isSafeGit in rules.go), so `PAGER=/tmp/evil git
	// log` would otherwise auto-approve an exec with no human present.
	"PAGER":     true,
	"EDITOR":    true,
	"VISUAL":    true,
	"LESSOPEN":  true,
	"LESSCLOSE": true,

	// Shell option state: SHELLOPTS/BASHOPTS are honoured by a child bash and
	// can enable xtrace/other behaviour the argv classifier never sees.
	"SHELLOPTS": true,
	"BASHOPTS":  true,
	"CDPATH":    true,
}

// dangerousEnvVarPrefixes catch whole hook families without enumerating them —
// enumeration is exactly how this denylist would silently rot.
//
//   - LD_ / DYLD_: dynamic-loader hooks (LD_PRELOAD, DYLD_INSERT_LIBRARIES,
//     DYLD_FRAMEWORK_PATH, and any future member). The enumerated LD_* entries
//     above are redundant with this rule and kept only so the list reads as the
//     documented denylist.
//   - GIT_: git is the one auto-approvable command with a large family of
//     "exec this program for me" env vars — GIT_EXTERNAL_DIFF, GIT_PAGER,
//     GIT_EDITOR, GIT_SEQUENCE_EDITOR, GIT_PROXY_COMMAND, GIT_SSH,
//     GIT_TEXTCONV_*, and GIT_CONFIG_COUNT/KEY_n/VALUE_n, which injects
//     arbitrary config (core.pager, core.sshCommand) with no file on disk.
//     `git log`/`git diff`/`git status` auto-approve (isSafeGit, rules.go), so
//     a missed GIT_* name is a silent exec with no human in the loop. Denying
//     the whole prefix costs a prompt on benign forms like GIT_AUTHOR_NAME —
//     which accompany `git commit`, itself never auto-approved anyway.
//   - BASH_: BASH_ENV runs a startup file for non-interactive bash, and the
//     BASH_FUNC_* family is the shellshock-style exported-function vector.
var dangerousEnvVarPrefixes = []string{"LD_", "DYLD_", "GIT_", "BASH_"}

// dangerousEnvVarName reports whether an env-var name is on the denylist or
// caught by a prefix rule. Matching is case-insensitive: a lowercase `ld_preload`
// is a different (inert) shell variable, but denying it too costs at most a human
// prompt, whereas a missed case costs a silent bypass. Fail closed on the tie.
//
// Shared with change 0070 D2's `env NAME=val` peeling — do not inline this.
func dangerousEnvVarName(name string) bool {
	upper := strings.ToUpper(name)
	if dangerousEnvVars[upper] {
		return true
	}
	for _, p := range dangerousEnvVarPrefixes {
		if strings.HasPrefix(upper, p) {
			return true
		}
	}
	return false
}

// assignsAreBenign reports whether every assignment prefix on a simple command
// is safe to drop and evaluate the remaining command on its own merits.
//
// An assignment is benign only when all of the following hold:
//
//   - it is the plain `NAME=value` shape — no `+=` append, no `NAME[i]=`
//     subscript, no `NAME=(array)`, and no naked `NAME` (declare/export forms).
//     Those are not needed to run a command and are cheaper to refuse than to
//     reason about. Only `+=` is reachable through an inline prefix today (the
//     parser rejects the array/subscript forms outright); the rest are guarded
//     because this helper is shared and must be safe on any Assign node;
//   - NAME is not caught by dangerousEnvVarName;
//   - the value is statically literal. `FOO=$(id)` and `FOO=$BAR` fail closed:
//     we cannot know what we would be setting, and therefore cannot know that
//     the name being benign is enough. A nil/absent value (bare `FOO=`) is
//     benign whenever the name is.
//
// Shared with change 0070 D2's `env NAME=val` peeling — do not inline this.
func assignsAreBenign(assigns []*syntax.Assign) bool {
	for _, a := range assigns {
		if a == nil || a.Name == nil {
			return false
		}
		if a.Append || a.Naked || a.Index != nil || a.Array != nil {
			return false
		}
		if dangerousEnvVarName(a.Name.Value) {
			return false
		}
		if a.Value == nil {
			continue
		}
		if _, ok := literalWord(a.Value); !ok {
			return false
		}
	}
	return true
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
// redirIsBenign, change 0037); an env-var assignment prefix that is not benign
// under assignsAreBenign (change 0070); an argv[0] containing a path separator
// (the basename-collapse bug); or an arbitrary-arg wrapper as argv[0] (bash/sh
// without a parseable -c, xargs, env, npx, timeout-then-unknown, sudo, …).
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
	// Env-var assignment prefixes (FOO=bar cmd). A prefix that cannot change
	// what code runs is dropped and the inner command is classified on its own
	// merits (change 0070 D1); anything else fails closed at the parse floor.
	// The assignments are DROPPED, never appended to Args: they are not
	// arguments, and a downstream path-scoper or deny matcher must not see them.
	if !assignsAreBenign(call.Assigns) {
		return ErrUnparseable
	}
	if len(call.Args) == 0 {
		// An assignment-only statement (`FOO=1`) runs no command, so there is
		// nothing for any layer to classify; likewise an empty call.
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
