package permissions

import (
	"errors"
	"path"
	"regexp"
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
	// WriteTargets are the literal file targets of the redirects attached to the
	// statement that produced this segment (`> out.log`, `2>> err.log`, `&>
	// build.log`). They are NOT arguments — no downstream deny matcher or
	// read-only check should see them as argv — but they ARE writes, and every
	// layer that decides whether a command mutates outside its roots must scope
	// them exactly as it scopes pathArgs (change 0070 D4).
	//
	// The parser records rather than decides here because it has no root set: a
	// target is only judgeable at classifyHeuristic, which holds the roots. Empty
	// means the statement wrote no file we could name — either it had no
	// redirect at all, or only benign ones (/dev/null, an fd-dup, an input
	// redirect, a here-doc). A target we could NOT name (`> $F`, `<>`, a
	// dup-to-path) never reaches here: it still fails the whole command closed.
	WriteTargets []string
	// Opaque is index-aligned with Args: Opaque[i] reports that Args[i] is an
	// OPAQUE argument — a word whose value is not statically known ($VAR,
	// ${VAR}, a read-only $(…), or any word mixing a literal with one of those).
	// The stored Args value is then the raw SOURCE TEXT of the word, kept so
	// deny/ask patterns and human-facing reasons can still name it. It is not,
	// and can never become, a value (change 0070 D5).
	//
	// THE INVARIANT this field exists to carry: an opaque argument must NEVER be
	// resolved as a filesystem path for a containment proof. Opaque is
	// *unprovable ⇒ ask*, never *resolves-under-cwd ⇒ allow*. Concretely it must
	// never reach withinWorkspace/resolveExisting (heuristics.go), which walk up
	// to the deepest existing ancestor and would happily let `rm $(echo /)`
	// prove containment against the cwd — the same failure family as the #0068
	// finding `containment-proof-needs-a-real-resolved-path`, where an
	// unexpanded `~` and a process-name operand did exactly that.
	//
	// REPRESENTATION: Opaque is either EMPTY (no argument is opaque — the shape
	// every pre-D5 construction site and hand-built test fixture produces) or
	// exactly len(Args) long. Never index it directly; go through isOpaque /
	// hasOpaqueArg, which fail TOWARD opaque on a length skew or an
	// out-of-range index. A raw index would silently read false on a skew, and
	// false is the fail-open direction.
	Opaque []bool
}

// isOpaque reports whether Args[i] is an opaque argument — a value the parser
// could name but never resolve. Every consumer that treats an argument as a
// path, a flag, a PID, or a URL must consult this first.
//
// It fails TOWARD opaque: an out-of-range index, or a Opaque/Args length skew,
// answers true. "I cannot tell" and "not opaque" must never be the same answer,
// because only one of them is safe.
func (s Segment) isOpaque(i int) bool {
	if len(s.Opaque) == 0 {
		return false // no opaque arguments were recorded for this segment.
	}
	if len(s.Opaque) != len(s.Args) {
		return true // parallel-slice skew: nothing here is provable.
	}
	if i < 0 || i >= len(s.Opaque) {
		return true
	}
	return s.Opaque[i]
}

// hasOpaqueArg reports whether any argument of the segment is opaque. It is the
// predicate classifyHeuristic turns into VerdictAsk for a mutating segment:
// there is nothing to prove containment OVER, and "no path args to check" must
// not read as "nothing to check".
func (s Segment) hasOpaqueArg() bool {
	if len(s.Opaque) == 0 {
		return false
	}
	if len(s.Opaque) != len(s.Args) {
		return true
	}
	for _, o := range s.Opaque {
		if o {
			return true
		}
	}
	return false
}

// argWords is a command's words carried alongside their opaqueness, so the two
// can never drift apart while the wrapper peels slice a prefix off the front.
// Keeping them in one value rather than two locals is the whole point: a peel
// that dropped a word from one slice and not the other would produce a segment
// whose Opaque flags describe the WRONG arguments — silently, and in the
// fail-open direction for every position that shifted from opaque to literal.
//
// Its invariant (established at construction, preserved by drop): len(text) ==
// len(opaque).
type argWords struct {
	text   []string
	opaque []bool
}

func (w argWords) len() int { return len(w.text) }

func (w argWords) at(i int) string { return w.text[i] }

// opaqueAt fails toward opaque on anything it cannot answer, exactly like
// Segment.isOpaque.
func (w argWords) opaqueAt(i int) bool {
	if len(w.opaque) != len(w.text) {
		return true
	}
	if i < 0 || i >= len(w.opaque) {
		return true
	}
	return w.opaque[i]
}

// drop returns the words after removing the first n, keeping both slices in
// lockstep.
func (w argWords) drop(n int) argWords {
	if n > len(w.text) {
		n = len(w.text)
	}
	if len(w.opaque) != len(w.text) {
		// Unreachable given the construction invariant; if it ever happens,
		// collapse to nothing rather than hand back a skewed value.
		return argWords{}
	}
	return argWords{text: w.text[n:], opaque: w.opaque[n:]}
}

// anyOpaque reports whether any word is opaque.
func (w argWords) anyOpaque() bool {
	if len(w.opaque) != len(w.text) {
		return true
	}
	for _, o := range w.opaque {
		if o {
			return true
		}
	}
	return false
}

// arbitraryArgWrappers are argv[0] names that run an arbitrary inner command
// supplied as data. They are never peeled and never inherit the inner
// command's allow decision, so their presence as a bare command fails closed.
// bash/sh are handled specially (a parseable `-c "<script>"` is peeled; any
// other form fails closed).
var arbitraryArgWrappers = map[string]bool{
	"xargs":  true,
	"npx":    true,
	"sudo":   true,
	"watch":  true,
	"docker": true,
	"eval":   true,
	"exec":   true,
}

// peelWrappers are side-effect-free prefixes stripped before the argv[0]
// check. Their first non-flag argument becomes the effective command, because
// every option they take is either valueless or attached (`nice -n5`); a
// leading `-` word is therefore never the inner command.
//
// timeout and env are NOT here: both carry operands that are not flags
// (timeout's mandatory duration, env's NAME=val prefixes) and both have flags
// that change what actually runs. They get dedicated peels — peelTimeout and
// peelEnv — precisely because peelWrapperArgs drops every leading `-` word
// blindly, which on `env -i cmd` would silently discard the fact that the
// environment was cleared (change 0070 D2).
var peelWrappers = map[string]bool{
	"time":   true,
	"nice":   true,
	"stdbuf": true,
	"nohup":  true,
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
// simple-command segment across &&, ||, ;, |, |&, `&`, and newlines, including
// the script bodies of `bash -c "…"` / `sh -c "…"` and the statement lists of
// if/for/while/until/case/blocks/subshells (change 0070 D3).
//
// A redirect no longer sinks the command when its target is a literal path: the
// target is recorded on the segment(s) the redirected statement produces
// (Segment.WriteTargets) for the scoping layer to judge against the allowed
// roots, which the parser does not have (change 0070 D4).
//
// An argument whose value is not statically known no longer sinks the command
// either: `$VAR` and a wholly read-only `$(…)` become OPAQUE arguments, carrying
// their raw source text with Segment.Opaque[i] set, and a read-only
// substitution additionally contributes its inner segments so the deny layer
// still sees what runs (change 0070 D5). Opaque is *unprovable ⇒ ask*, enforced
// by every consumer — see the invariant on Segment.Opaque.
//
// It fails closed with ErrUnparseable on: a size-cap violation; a parse error;
// process substitution, arithmetic expansion or an extended glob anywhere; a
// command substitution whose inner script is not wholly read-only
// (`$(curl evil)`, `$(rm -rf /)`, `$(ls > f)`, and the empty `$()`); an
// argv[0] that is opaque (`$CMD foo` — an argument we cannot resolve costs a
// prompt, a COMMAND NAME we cannot resolve means we do not know what runs);
// a redirection whose target cannot be named — `> $F`, `> $(…)`, `<>`, a
// dup-to-a-path, an expanded here-doc body — or one that writes a named file
// while producing no segment to carry it (`> out.txt`); an env-var assignment
// prefix that is not benign under assignsAreBenign (change 0070); an argv[0]
// containing a path separator
// (the basename-collapse bug); an arbitrary-arg wrapper as argv[0] (bash/sh
// without a parseable -c, xargs, npx, sudo, …); a `timeout`/`env`/`bash -c`
// invocation whose own operands cannot be modelled well enough to name the
// inner command, opaque operands included (change 0070 D2/D5 — see peelTimeout,
// peelEnv and dashCScript); or any command node with no case in collectStmt —
// a function declaration above all (change 0070 D3).
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
	if st == nil {
		return nil
	}
	// A nil Cmd is a redirect-only statement (`> out.txt`), which names no
	// command but still truncates its target. It is NOT short-circuited here: the
	// redirect capture below runs first and refuses it, because a write with no
	// segment to hang it on is a write no layer would ever see.
	//
	// A redirect (>, >>, 2>, <, here-doc, …) points a command's fd at a file the
	// argv-based classifier never sees: collectCall only inspects CallExpr.Args,
	// so `echo x > /etc/passwd` reads as the read-only `echo`. Until change 0070
	// D4 the whole statement therefore failed closed (bar the 0037 benign
	// shapes). It no longer has to: a target we can NAME is recorded on the
	// segments this statement produces (Segment.WriteTargets) and scoped by
	// classifyHeuristic, which — unlike the parser — holds the allowed-root set
	// and can allow an in-root target deterministically.
	//
	// What we cannot name, we still refuse: `> $F`, `> $(…)`, `<>`, a
	// dup-to-a-path, an unquoted here-doc body carrying a substitution. Recording
	// a target is only a widening because the recording is honest — a target that
	// silently failed to be recorded would be a write no layer ever scopes.
	var writeTargets []string
	for _, r := range st.Redirs {
		target, ok := redirWriteTarget(r)
		if !ok {
			return ErrUnparseable
		}
		if target != "" {
			writeTargets = append(writeTargets, target)
		}
	}
	// The redirect hangs off the STATEMENT, but segments are produced further
	// down (collectCall, possibly several levels deep through a block, a loop
	// body or a `bash -c` script). Remember where this statement's segments start
	// so the targets can be attached to every one of them afterwards: a compound
	// statement's redirect applies to every command inside it, so attaching to
	// only the first would leave the rest looking like pure readers.
	start := len(*out)
	if st.Cmd != nil {
		if err := collectStmtCmd(src, st, out); err != nil {
			return err
		}
	}
	if len(writeTargets) == 0 {
		return nil
	}
	if len(*out) == start {
		// The statement wrote a file but named no command we can classify
		// (`> out.txt`, `FOO=1 > out.txt`, `env FOO=1 > out.txt` — all of which
		// truncate the target). There is no segment to carry the target, so the
		// write would be invisible to every downstream layer. Fail closed.
		return ErrUnparseable
	}
	for i := start; i < len(*out); i++ {
		(*out)[i].WriteTargets = append((*out)[i].WriteTargets, writeTargets...)
	}
	return nil
}

// collectStmtCmd dispatches on a statement's command node. It is split out of
// collectStmt so the redirect capture there can bracket the whole descent — see
// the WriteTargets plumbing above — rather than being threaded through every
// arm of this switch.
func collectStmtCmd(src string, st *syntax.Stmt, out *[]Segment) error {
	// st.Background (`cmd &`) is deliberately not consulted: backgrounding
	// changes WHEN a command runs, not WHAT runs, so the segments are identical
	// and the classifier's decision must be too. Same for st.Negated (`! cmd`).
	switch cmd := st.Cmd.(type) {
	case *syntax.BinaryCmd:
		// &&, ||, | and |& all decompose into their operands.
		if err := collectStmt(src, cmd.X, out); err != nil {
			return err
		}
		return collectStmt(src, cmd.Y, out)
	case *syntax.CallExpr:
		return collectCall(src, cmd, out)

	// Control-flow constructs carry statement lists rather than being commands
	// themselves. Descending EVERY list they carry — not just the obvious body
	// — is what makes this safe: each descended statement faces the same
	// redirect guard, wrapper check, path-qualified-argv[0] check and env-prefix
	// denylist as a top-level statement, so the classifier sees every command
	// that could run. Missing one list would hide a command from every
	// downstream layer, which is strictly worse than the old blanket
	// fail-closed (change 0070 D3).
	case *syntax.IfClause:
		return collectIf(src, cmd, out)
	case *syntax.ForClause:
		if err := collectLoopHeader(src, cmd.Loop, out); err != nil {
			return err
		}
		return collectStmts(src, cmd.Do, out)
	case *syntax.WhileClause:
		// `until` is this same node with Until: true; the statement lists are
		// identical, so the descent is too.
		if err := collectStmts(src, cmd.Cond, out); err != nil {
			return err
		}
		return collectStmts(src, cmd.Do, out)
	case *syntax.CaseClause:
		return collectCase(src, cmd, out)
	case *syntax.Block:
		return collectStmts(src, cmd.Stmts, out)
	case *syntax.Subshell:
		// A subshell isolates state (cd, exported vars), not effects: the
		// commands inside still touch the same filesystem.
		return collectStmts(src, cmd.Stmts, out)

	default:
		// Everything with no case above — *syntax.FuncDecl above all, plus
		// TestClause ([[ ]]), ArithmCmd, LetClause, TimeClause, CoprocClause —
		// is not something we can decompose safely: fail closed.
		//
		// FuncDecl MUST STAY HERE. A declaration's body does not run when it is
		// declared, its call sites are invisible to an argv-based classifier,
		// and it is the recursion vehicle for the fork bomb `:(){ :|:& };:`.
		// Descending it would report the body's segments as if they were what
		// runs, which is a lie in both directions. Do not "finish the job" by
		// adding a FuncDecl case — TestSplitSegments_FailClosed pins this.
		return ErrUnparseable
	}
}

// collectIf descends an if statement's condition, its then-branch, and its
// entire else chain. In the v3 AST an `elif`/`else` is itself an *IfClause
// hanging off .Else (an `else` being the degenerate one with an empty Cond), so
// the chain is followed to its end — stopping at the first level would leave a
// trailing `else rm -rf /` invisible.
func collectIf(src string, cmd *syntax.IfClause, out *[]Segment) error {
	for c := cmd; c != nil; c = c.Else {
		if err := collectStmts(src, c.Cond, out); err != nil {
			return err
		}
		if err := collectStmts(src, c.Then, out); err != nil {
			return err
		}
	}
	return nil
}

// collectCase descends a case statement's discriminant, every branch's
// patterns, and every branch's statement list. Discriminant and patterns are
// words, not commands, so they produce no segment themselves — but they take
// exactly the same literal-or-opaque discipline as any argument (change 0070
// D5, lifting D3's interim literal-only posture). `case $x in a) …` is now
// resolvable-enough: the discriminant selects a branch, and every command in
// every branch is enumerated regardless of which one it selects. A pattern or
// discriminant carrying a command substitution still surfaces that
// substitution's segments into out, so `case $(curl evil) in …` reaches the
// deny layer as a curl.
func collectCase(src string, cmd *syntax.CaseClause, out *[]Segment) error {
	if cmd.Word == nil {
		return ErrUnparseable
	}
	if _, _, ok := classifyWord(src, cmd.Word, out); !ok {
		return ErrUnparseable
	}
	for _, item := range cmd.Items {
		if item == nil {
			return ErrUnparseable
		}
		for _, pat := range item.Patterns {
			if pat == nil {
				return ErrUnparseable
			}
			if _, _, ok := classifyWord(src, pat, out); !ok {
				return ErrUnparseable
			}
		}
		if err := collectStmts(src, item.Stmts, out); err != nil {
			return err
		}
	}
	return nil
}

// collectLoopHeader validates a for-clause header. The header itself names no
// command, but its words take the same literal-or-opaque discipline as any
// argument (change 0070 D5, lifting D3's interim literal-only posture).
//
//   - *syntax.WordIter is the `for f in a b` form. An item word may be literal
//     or opaque: `for f in $LIST` no longer sinks the command, because the loop
//     VARIABLE is itself opaque wherever the body uses it (`rm $f` carries an
//     opaque arg and asks), so nothing is proven by resolving the header. An
//     item carrying a command substitution surfaces that substitution's
//     segments into out — `for f in $(ls)` contributes an `ls` segment — and a
//     substitution that is not read-only still fails the whole command closed.
//     An absent `in` list (`for f; do …`, which iterates the positional
//     parameters) has no items and nothing to resolve.
//   - *syntax.CStyleLoop is `for ((i=0;i<10;i++))`. It carries arithmetic
//     expressions, not words: nothing here can be resolved statically, and the
//     body-only descent that would otherwise apply is not worth the risk of
//     silently ignoring a header we did not model. Fail closed.
func collectLoopHeader(src string, loop syntax.Loop, out *[]Segment) error {
	iter, ok := loop.(*syntax.WordIter)
	if !ok {
		return ErrUnparseable
	}
	for _, item := range iter.Items {
		if item == nil {
			return ErrUnparseable
		}
		if _, _, ok := classifyWord(src, item, out); !ok {
			return ErrUnparseable
		}
	}
	return nil
}

// redirWriteTarget classifies one redirect for change 0070 D4.
//
//	ok=false        ⇒ the redirect cannot be modelled; the whole command fails closed.
//	ok, target==""  ⇒ benign: no file is written that any layer needs to scope.
//	ok, target!=""  ⇒ the literal path this redirect writes, to be recorded on
//	                  the statement's segments and scoped against the roots.
//
// redirIsBenign (change 0037) stays the fast path, so /dev/null and fd-dups keep
// their existing "nothing to see here" answer with no change in behaviour; only
// what IT rejects is examined further here.
//
// Benign beyond that fast path:
//
//   - RdrIn from a literal file (`cat < in.txt`). Reading a file is not a
//     mutation, and nothing downstream needs to prove containment over a read.
//   - A here-doc (`<<EOF` / `<<-EOF`) whose delimiter and body are statically
//     literal. The body is data fed on stdin, not a file target. The literal
//     requirement is NOT ceremony: an unquoted here-doc body is expanded by the
//     shell before it is fed in, so `<<EOF` with a `$(rm -rf /)` in the body
//     genuinely runs that command — invisibly to an argv classifier.
//
// Fail-closed, deliberately: a non-literal target (`> $F`, `> $(…)`) names a
// file we cannot scope; RdrInOut (`<>`) opens for writing; a dup-to-a-path
// (`>&file`) and a fd-close (`2>&-`) are neither a plain write nor a plain dup;
// and a here-string (`<<<`) falls through to the default arm.
func redirWriteTarget(r *syntax.Redirect) (string, bool) {
	if redirIsBenign(r) {
		return "", true
	}
	if r == nil || r.Word == nil {
		return "", false
	}
	switch r.Op {
	case syntax.Hdoc, syntax.DashHdoc:
		if _, ok := literalWord(r.Word); !ok {
			return "", false // a non-literal delimiter is a body we cannot read.
		}
		if r.Hdoc != nil {
			if _, ok := literalWord(r.Hdoc); !ok {
				return "", false
			}
		}
		return "", true
	case syntax.RdrIn:
		if _, ok := literalWord(r.Word); !ok {
			return "", false
		}
		return "", true
	case syntax.RdrOut, syntax.AppOut, syntax.RdrAll, syntax.AppAll:
		// A here-doc body can only hang off a here-doc op, but this helper is
		// shared: a write op carrying one is a shape we did not model.
		if r.Hdoc != nil {
			return "", false
		}
		target, ok := literalWord(r.Word)
		if !ok || target == "" {
			return "", false
		}
		return target, true
	default:
		return "", false
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

	// Classify every word as literal or OPAQUE, failing closed on any expansion
	// with no honest opaque representation (process substitution, arithmetic,
	// extended globs) and on a command substitution whose inner script is not
	// wholly read-only (change 0070 D5).
	words := argWords{
		text:   make([]string, 0, len(call.Args)),
		opaque: make([]bool, 0, len(call.Args)),
	}
	for _, w := range call.Args {
		text, opaque, ok := classifyWord(src, w, out)
		if !ok {
			return ErrUnparseable
		}
		words.text = append(words.text, text)
		words.opaque = append(words.opaque, opaque)
	}

	// Peel side-effect-free wrappers; the peeled remainder must itself be a
	// bare, non-path-qualified command. Peeling repeats so nested wrappers
	// (`nohup env FOO=1 timeout 30 go build`) reduce to the real command, and a
	// path-qualified wrapper (`/usr/bin/timeout …`) is never peeled — it falls
	// through to the path-qualified check below and fails closed.
	//
	// Everything the loop leaves behind still faces the post-peel checks: a
	// path-qualified argv[0] fails closed, bash/sh re-enters the `-c` path, and
	// an inner command that is itself an arbitrary-arg wrapper
	// (`timeout 30 sudo rm -rf /`) fails closed on arbitraryArgWrappers.
	//
	// argv[0] is re-checked for opaqueness on EVERY iteration, not just the
	// first: `nohup $CMD` and `env FOO=1 $CMD` only expose their opaque command
	// name after a peel. An opaque argument costs a human prompt; an opaque
	// COMMAND NAME means we do not know what runs at all, and every name-keyed
	// table below (arbitraryArgWrappers, and downstream the deny names,
	// readOnlyUtils, the egress list) would be consulted with a name that is
	// not the one bash will execute.
peel:
	for {
		if words.len() == 0 {
			return ErrUnparseable
		}
		if words.opaqueAt(0) {
			return ErrUnparseable
		}
		if strings.ContainsRune(words.at(0), '/') {
			break peel
		}
		switch base := basename(words.at(0)); {
		case peelWrappers[base]:
			peeled, ok := peelWrapperArgs(words)
			if !ok {
				return ErrUnparseable
			}
			words = peeled
		case base == "timeout":
			peeled, ok := peelTimeout(words)
			if !ok {
				return ErrUnparseable
			}
			words = peeled
		case base == "env":
			peeled, ok := peelEnv(words)
			if !ok {
				return ErrUnparseable
			}
			if peeled.len() == 0 {
				// `env` alone, or `env FOO=1`, prints the environment and runs
				// no command: benign, and there is nothing to classify. Produce
				// no segment rather than failing closed.
				return nil
			}
			words = peeled
		default:
			break peel
		}
	}

	argv0 := words.at(0)

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
		script, ok := dashCScript(words.drop(1))
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

	args := words.drop(1)
	seg := Segment{
		Name: name,
		Args: append([]string(nil), args.text...),
		Raw:  rawSegment(src, call),
	}
	// Opaque stays EMPTY when nothing is opaque, so a segment that carries no
	// unresolvable word is byte-for-byte the pre-D5 shape and every hand-built
	// Segment keeps reading as fully literal (see Segment.isOpaque).
	if args.anyOpaque() {
		seg.Opaque = append([]bool(nil), args.opaque...)
	}
	if len(seg.Args) == 0 {
		seg.Args = nil
		seg.Opaque = nil
	}
	*out = append(*out, seg)
	return nil
}

// classifyWord resolves one word into either a literal value or an OPAQUE one
// (change 0070 D5). It returns the word's text, whether that text is opaque,
// and ok=false when the word has no honest representation at all and the whole
// command must fail closed.
//
// The three answers, and why they are three rather than two:
//
//   - LITERAL: every part resolves statically. The text IS the value, and a
//     downstream layer may resolve it as a path, read it as a flag, or parse it
//     as a PID.
//   - OPAQUE: the word contains a parameter expansion ($VAR, ${VAR}) or a
//     read-only command substitution. The text is the word's RAW SOURCE, kept
//     so a deny pattern and a human-facing reason can still name it — it is not
//     a value and must never be resolved as one. Before D5 this whole class
//     fell into fail-closed, which cost a human prompt on `wc -l $f`.
//   - FAIL CLOSED: a process substitution, an arithmetic expansion, an extended
//     glob, a substitution running something that is not read-only, or a word
//     whose source text we cannot even quote back. None of these can be
//     represented as "a value we cannot resolve" — a ProcSubst names a
//     filesystem object, and a non-read-only substitution RUNS.
//
// A word that MIXES a literal with an expansion (`--out=$DIR/x`, `"pre$VAR"`)
// is opaque as a whole. It is never partially resolved: a half-resolved path is
// the containment bypass in miniature — `$(echo ~)/x` would otherwise present a
// resolvable-looking `/x` tail hanging off an unknown root.
//
// A command substitution's inner statements are APPENDED to out, so the deny
// layer still sees `$(curl evil)` as a curl segment rather than as a string.
// They are the same AST the outer parse produced, so their positions index the
// same src; there is no re-parse.
func classifyWord(src string, w *syntax.Word, out *[]Segment) (string, bool, bool) {
	if w == nil {
		return "", false, false
	}
	return classifyWordParts(src, w.Parts, out)
}

func classifyWordParts(src string, parts []syntax.WordPart, out *[]Segment) (string, bool, bool) {
	var b strings.Builder
	opaque := false
	for _, part := range parts {
		switch p := part.(type) {
		case *syntax.Lit:
			b.WriteString(p.Value)
		case *syntax.SglQuoted:
			b.WriteString(p.Value)
		case *syntax.DblQuoted:
			// A double-quoted run can itself carry expansions; classify its
			// parts on exactly the same terms so `"pre$VAR"` is opaque rather
			// than fail-closed, and `"literal"` stays literal.
			s, innerOpaque, ok := classifyWordParts(src, p.Parts, out)
			if !ok {
				return "", false, false
			}
			opaque = opaque || innerOpaque
			b.WriteString(s)
		case *syntax.ParamExp:
			text, ok := nodeText(src, p)
			if !ok {
				return "", false, false
			}
			opaque = true
			b.WriteString(text)
		case *syntax.CmdSubst:
			// mksh's ${ …;} / ${| …;} value-substitution forms are shapes we did
			// not model; they should not reach us under LangBash, and if they
			// ever do they are not the plain $(…) this arm reasons about.
			if p.TempFile || p.ReplyVar {
				return "", false, false
			}
			var inner []Segment
			if err := collectStmts(src, p.Stmts, &inner); err != nil {
				return "", false, false
			}
			// A substitution RUNS its contents, so it earns the opaque
			// treatment only when everything inside it is wholly read-only —
			// which also rejects an inner statement carrying a redirect write
			// target, and rejects an EMPTY substitution (nothing was proven
			// read-only, so nothing is). `$(git rev-parse --show-toplevel)`
			// qualifies; `$(curl evil)` and `$(rm -rf /)` do not and sink the
			// whole command as they did before D5.
			if !allSegmentsReadOnlySafe(inner) {
				return "", false, false
			}
			text, ok := nodeText(src, p)
			if !ok {
				return "", false, false
			}
			// Surface the inner command to the deny/ask layers. It is appended
			// BEFORE the outer segment, so `git diff $(git rev-parse …)` reads
			// as [git rev-parse, git diff].
			*out = append(*out, inner...)
			opaque = true
			b.WriteString(text)
		case *syntax.ProcSubst, *syntax.ArithmExp, *syntax.ExtGlob:
			// Unchanged from before D5: a process substitution names a
			// filesystem object we would have to model, and arithmetic /
			// extended globs are neither a value nor a command.
			return "", false, false
		default:
			return "", false, false
		}
	}
	return b.String(), opaque, true
}

// nodeText returns the original source text spanning a node, used to give an
// opaque word a name a human and a deny pattern can read. ok=false when the
// offsets do not lie inside src — in which case the caller fails closed rather
// than invent a token, because every downstream layer would then be matching
// against text that is not what the shell will see.
func nodeText(src string, n syntax.Node) (string, bool) {
	start := int(n.Pos().Offset())
	end := int(n.End().Offset())
	if start < 0 || end > len(src) || start >= end {
		return "", false
	}
	return src[start:end], true
}

// literalWord returns the fully-literal string value of a word, or ok=false if
// the word contains ANY expansion — a substitution, a parameter expansion,
// arithmetic, an extended glob. Quoting resolves to the quoted text.
//
// Since change 0070 D5 this is the STRICT half of a pair, and it is
// deliberately not the one collectCall uses. Command ARGUMENTS go through
// classifyWord, which can answer "opaque" — a value we can name but not
// resolve. literalWord serves the three positions where opaque has no honest
// meaning and there is nothing to widen:
//
//   - a redirect target (redirWriteTarget/redirIsBenign): an unnameable write
//     is a write no layer can scope, so it must sink the command;
//   - a here-doc delimiter and body: an unquoted body is EXPANDED before it is
//     fed to stdin, so a substitution in it genuinely runs;
//   - an env-assignment value (assignsAreBenign): a benign NAME proves nothing
//     when we cannot know what we would be setting it to.
//
// Do not "unify" these onto classifyWord. The three callers above need
// ok=false, not an opaque token.
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
// An opaque word anywhere it inspects fails closed: `bash -c $SCRIPT` runs a
// script we cannot read, so there is nothing to decompose and nothing to
// classify (change 0070 D5).
func dashCScript(args argWords) (string, bool) {
	for i := 0; i < args.len(); i++ {
		if args.opaqueAt(i) {
			return "", false
		}
		a := args.at(i)
		if a == "-c" {
			if i+1 >= args.len() || args.opaqueAt(i+1) {
				return "", false
			}
			return args.at(i + 1), true
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
//
// An OPAQUE word in the flag position fails closed (change 0070 D5): a word
// like `-$X` looks like a flag but we cannot know that it is one, and dropping
// it would silently discard something that may have been the command. An
// opaque word that does NOT look like a flag ends the loop and becomes argv[0],
// where collectCall's opaque-argv[0] check refuses it.
func peelWrapperArgs(words argWords) (argWords, bool) {
	rest := words.drop(1)
	for rest.len() > 0 && strings.HasPrefix(rest.at(0), "-") {
		if rest.opaqueAt(0) {
			return argWords{}, false
		}
		rest = rest.drop(1)
	}
	return rest, true
}

// timeoutDuration matches timeout's mandatory DURATION operand: a decimal
// number with an optional s/m/h/d unit suffix (`30`, `1.5s`, `10m`). Anything
// else in that position is not a duration we modelled, and the whole command
// fails closed rather than guess which word is the inner command.
var timeoutDuration = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?[smhd]?$`)

// peelTimeout strips `timeout`'s own flags and its mandatory duration operand,
// returning the inner command's words. words[0] is the timeout invocation
// itself (change 0070 D2).
//
// This is deliberately NOT peelWrapperArgs. timeout's `-s`/`-k` take a value as
// a SEPARATE word, so blindly dropping leading `-` words would leave that value
// sitting where the duration belongs (`timeout -s 30 make` would peel to
// `make`, having silently eaten the signal name and mistaken the duration for
// it). We therefore model each flag's arity explicitly and fail closed on any
// flag we did not model — an unknown flag might take a separate value too.
//
// After the flags, exactly one duration-shaped word must appear. Zero (`timeout
// go test`) fails closed; so does a second duration-shaped word, since no real
// command is named like a duration and that shape means we misread the flags.
// An OPAQUE flag or duration fails closed (change 0070 D5): `timeout $T make`
// gives us no way to tell the duration operand from the command, and a flag we
// cannot read is a flag whose arity we cannot model. A flag's separate VALUE
// may be opaque — `timeout -s $SIG make` names a signal, not a command, and
// dropping it is correct either way.
func peelTimeout(words argWords) (argWords, bool) {
	rest := words.drop(1)
	for rest.len() > 0 && strings.HasPrefix(rest.at(0), "-") {
		if rest.opaqueAt(0) {
			return argWords{}, false
		}
		flag := rest.at(0)
		switch {
		case flag == "--preserve-status" || flag == "--foreground":
			rest = rest.drop(1)
		case flag == "-s" || flag == "-k" || flag == "--signal" || flag == "--kill-after":
			// Value in a separate word: drop both.
			if rest.len() < 2 {
				return argWords{}, false
			}
			rest = rest.drop(2)
		case strings.HasPrefix(flag, "--signal=") || strings.HasPrefix(flag, "--kill-after="):
			rest = rest.drop(1)
		case len(flag) > 2 && (strings.HasPrefix(flag, "-s") || strings.HasPrefix(flag, "-k")):
			// Attached short-option value: -sKILL, -k5s.
			rest = rest.drop(1)
		default:
			return argWords{}, false
		}
	}
	if rest.len() == 0 || rest.opaqueAt(0) || !timeoutDuration.MatchString(rest.at(0)) {
		return argWords{}, false
	}
	rest = rest.drop(1)
	if rest.len() > 0 && !rest.opaqueAt(0) && timeoutDuration.MatchString(rest.at(0)) {
		return argWords{}, false
	}
	return rest, true
}

// peelEnv strips an `env` invocation's leading NAME=val assignments, returning
// the inner command's words. words[0] is the env invocation itself. An empty
// remainder with ok=true means `env` printed the environment and ran no command
// (change 0070 D2).
//
// ANY flag fails closed — `-i`, `-u`, `-S`, `-0`, a bare `--`, and every long
// form. This is deliberately blunt rather than an arity model like peelTimeout's:
// `env -i cmd` clears the environment (so the absence of a dangerous assignment
// proves nothing about what cmd will load) and `env -S 'foo bar'` re-splits its
// operand into an entirely different command. Neither is visible to the argv
// classifier downstream, and neither is common enough to be worth the risk of
// modelling.
//
// The assignment words are validated by the same rules as an inline `FOO=1 cmd`
// prefix: the D1 name denylist (dangerousEnvVarName — shared, never duplicated)
// plus a literal value. The literal half needs no check here because the caller
// already required every word to be a literalWord, so `env FOO=$(id) make` has
// failed closed before we are reached. Validated assignments are DROPPED, not
// passed on as arguments.
// An OPAQUE word in the region env's own operands occupy fails closed (change
// 0070 D5). This is stricter than the opaque-argv[0] check alone, and
// deliberately so: `env $X make` gives us no way to tell whether `$X` is an
// assignment prefix (leaving `make` as the command) or the command itself, and
// the two readings put different names in front of every downstream table.
func peelEnv(words argWords) (argWords, bool) {
	rest := words.drop(1)
	for rest.len() > 0 {
		if rest.opaqueAt(0) {
			return argWords{}, false
		}
		w := rest.at(0)
		if strings.HasPrefix(w, "-") {
			return argWords{}, false
		}
		eq := strings.IndexByte(w, '=')
		if eq < 0 {
			// First non-assignment word: the inner command starts here, and its
			// own flags are none of env's business.
			break
		}
		name := w[:eq]
		if !isEnvVarName(name) || dangerousEnvVarName(name) {
			return argWords{}, false
		}
		rest = rest.drop(1)
	}
	return rest, true
}

// isEnvVarName reports whether s has the shape of a shell variable name. env
// itself is laxer (it accepts any word containing '='), but a word we cannot
// read as a name is one we cannot reason about, so it fails closed.
func isEnvVarName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_':
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
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
