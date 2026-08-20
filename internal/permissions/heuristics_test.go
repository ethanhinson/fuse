package permissions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestClassifyHeuristic_ReadOnly proves wholly read-only commands classify as
// allow regardless of workspace root: no path scoping is needed for a read.
func TestClassifyHeuristic_ReadOnly(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		desc string
		cmd  string
		want Verdict
	}{
		{"git status is read-only allow", "git status", VerdictAllow},
		{"ls is read-only allow", "ls -la", VerdictAllow},
		{"compound read-only allow", "git status && git log", VerdictAllow},
		{"sed -n print is read-only allow", "sed -n 1,5p file", VerdictAllow},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			segs := segsFor(t, tc.cmd)
			if got := classifyHeuristic(segs, []string{root}); got != tc.want {
				t.Errorf("classifyHeuristic(%q) = %v, want %v", tc.cmd, got, tc.want)
			}
		})
	}
}

// TestClassifyHeuristic_Egress proves every network-egress command is a hard
// risk boundary that classifies as ask, never a silent allow — even if it would
// otherwise look benign.
func TestClassifyHeuristic_Egress(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		desc string
		cmd  string
	}{
		{"curl is egress ask", "curl https://example.com"},
		{"wget is egress ask", "wget https://example.com/x"},
		{"nc is egress ask", "nc example.com 80"},
		{"ssh is egress ask", "ssh host uptime"},
		{"scp is egress ask", "scp file host:/tmp"},
		{"git fetch a host remote is egress ask", "git fetch https://github.com/x/y"},
		{"git pull a host remote is egress ask", "git pull git@github.com:x/y"},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			segs := segsFor(t, tc.cmd)
			if got := classifyHeuristic(segs, []string{root}); got != VerdictAsk {
				t.Errorf("classifyHeuristic(%q) = %v, want VerdictAsk (egress)", tc.cmd, got)
			}
		})
	}
}

// TestClassifyHeuristic_InWorkspaceMutatingAllowed proves that a mutating file
// operation whose path argument resolves inside the workspace root classifies
// as allow. The command is run with the workspace root as its scope.
func TestClassifyHeuristic_InWorkspaceMutatingAllowed(t *testing.T) {
	root := t.TempDir()
	// An in-workspace file to touch. cp is a mutating (not read-only) command,
	// so it exercises the path-scoping branch rather than the read-only branch.
	src := filepath.Join(root, "src.txt")
	if err := os.WriteFile(src, []byte("hi"), 0o644); err != nil {
		t.Fatalf("seed src: %v", err)
	}

	segs := segsFor(t, "cp src.txt dst.txt")
	// The segments carry relative paths; classification resolves them against
	// the workspace root.
	got := classifyHeuristicIn(t, segs, root)
	if got != VerdictAllow {
		t.Errorf("classifyHeuristic(cp src.txt dst.txt) in workspace = %v, want VerdictAllow", got)
	}
}

// TestClassifyHeuristic_SymlinkEscapeCaught proves the CRITICAL learning: a
// symlink that lives INSIDE the workspace but points OUTSIDE it must be caught
// by resolving the link (EvalSymlinks), not by a lexical prefix check on the
// pre-resolution path. Writing through the link would escape the workspace.
func TestClassifyHeuristic_SymlinkEscapeCaught(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir() // a sibling temp dir, definitely outside root.
	outTarget := filepath.Join(outside, "target.txt")
	if err := os.WriteFile(outTarget, []byte("secret"), 0o644); err != nil {
		t.Fatalf("seed outside target: %v", err)
	}
	// escape lives in the workspace but its target is outside it. A naive
	// lexical prefix check on "escape.txt" (which is in-workspace) would wrongly
	// allow; resolving the link must catch the escape.
	link := filepath.Join(root, "escape.txt")
	if err := os.Symlink(outTarget, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	segs := segsFor(t, "cp innocent.txt escape.txt")
	// Seed the innocent source so it does not itself force a decision.
	if err := os.WriteFile(filepath.Join(root, "innocent.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed innocent: %v", err)
	}
	got := classifyHeuristicIn(t, segs, root)
	if got == VerdictAllow {
		t.Errorf("classifyHeuristic(cp innocent.txt escape.txt) = VerdictAllow, want ask/deny (symlink escapes workspace)")
	}
}

// TestClassifyHeuristic_MutatingOutsideRootCaught proves an absolute path
// argument that lies plainly outside the workspace is caught (no symlink
// needed) — the ordinary scoping boundary.
func TestClassifyHeuristic_MutatingOutsideRootCaught(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	dst := filepath.Join(outside, "dst.txt")

	segs := segsFor(t, "cp "+filepath.Join(root, "src.txt")+" "+dst)
	if err := os.WriteFile(filepath.Join(root, "src.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed src: %v", err)
	}
	got := classifyHeuristicIn(t, segs, root)
	if got == VerdictAllow {
		t.Errorf("classifyHeuristic(cp … outside) = VerdictAllow, want ask/deny (escapes workspace)")
	}
}

// TestClassifyHeuristic_LeafNotYetExisting proves a to-be-created leaf inside
// the workspace (no such file yet) still classifies as allow: scoping resolves
// the deepest existing ancestor, which is the in-workspace directory.
func TestClassifyHeuristic_LeafNotYetExisting(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "src.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed src: %v", err)
	}
	// dst does not exist yet; its parent (sub) is in-workspace.
	segs := segsFor(t, "cp src.txt sub/newfile.txt")
	got := classifyHeuristicIn(t, segs, root)
	if got != VerdictAllow {
		t.Errorf("classifyHeuristic(cp src.txt sub/newfile.txt) = %v, want VerdictAllow (leaf parent in-workspace)", got)
	}
}

// classifyHeuristicIn runs classifyHeuristic with cwd set to root so that the
// relative path arguments in the segment resolve against the workspace. It
// restores cwd on cleanup.
func classifyHeuristicIn(t *testing.T, segs []Segment, root string) Verdict {
	t.Helper()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir %q: %v", root, err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
	// EvalSymlinks canonicalizes root (e.g. /var -> /private/var on macOS), so
	// pass the canonical form the classifier will compare against.
	canonRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("evalsymlinks root: %v", err)
	}
	return classifyHeuristic(segs, []string{canonRoot})
}

// TestClassifyHeuristic_RedirectWriteTargets is where change 0070 D4's widening
// is actually made safe. The parser now records a redirect's literal target on
// the segment instead of refusing the command; this layer is the one holding the
// root set, so it is the only layer that can decide whether that target is
// acceptable. A target inside the roots is a deterministic allow; a target
// outside them is an ask, routed on to the classifier.
//
// Two ordering properties are load-bearing and each has a row below: the target
// scoping must run BEFORE the read-only fast path (a read-only command with a
// write target is a mutation — `echo x > /etc/passwd` is the whole point of the
// task), and before the kill / loopback-fetch `continue`s, which would otherwise
// carry a write target past the scoping untouched.
func TestClassifyHeuristic_RedirectWriteTargets(t *testing.T) {
	cases := []struct {
		desc string
		cmd  string
		want Verdict
	}{
		{"read-only echo with an out-of-root target is a mutation ⇒ ask", "echo x > /etc/passwd", VerdictAsk},
		{"append to a home dotfile ⇒ ask", "grep foo bar >> ~/.zshrc", VerdictAsk},
		{"relative escape target ⇒ ask", "ls > ../escaped.txt", VerdictAsk},
		{"build output inside the roots ⇒ allow", "go build > out.log", VerdictAllow},
		{"read-only command with an in-root target ⇒ allow", "ls > out.txt", VerdictAllow},
		{"in-root target on a compound statement ⇒ allow", "{ ls; wc -l x; } > out.txt", VerdictAllow},
		{"one out-of-root target among several ⇒ ask", "ls > out.txt 2> /etc/err.log", VerdictAsk},
		{"benign /dev/null carries no target ⇒ allow", "wc -l x 2>/dev/null", VerdictAllow},
		{"fd-dup carries no target ⇒ allow", "ls 2>&1", VerdictAllow},
		{"input redirect is a read ⇒ allow", "cat < config.yaml", VerdictAllow},
		{"here-doc is stdin data ⇒ allow", "cat <<EOF\nhi\nEOF", VerdictAllow},
		// The kill family short-circuits path scoping because its operands are
		// PIDs, not paths (change 0068) — but its REDIRECT is still a file write.
		{"a benign kill with an out-of-root target ⇒ ask", "kill 123 > /etc/passwd", VerdictAsk},
		{"a benign kill with an in-root target ⇒ allow", "kill 123 > kill.log", VerdictAllow},
		// Likewise the loopback-fetch allowance (change 0068): it exempts URL
		// operands from scoping, never a redirect target.
		{"a loopback fetch with an out-of-root target ⇒ ask", "curl -s http://localhost:8080/x > /etc/passwd", VerdictAsk},
		{"a loopback fetch with an in-root target ⇒ allow", "curl -s http://localhost:8080/x > body.json", VerdictAllow},
		// Unaffected shapes: `tee` names its output as an ARGUMENT, which the
		// pre-existing pathArgs scoping already covers.
		{"tee to an in-root file is unaffected", "ls | tee out.log", VerdictAllow},
		{"tee to an out-of-root file is unaffected", "ls | tee /etc/out.log", VerdictAsk},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			root := t.TempDir()
			segs := segsFor(t, tc.cmd)
			if got := classifyHeuristicIn(t, segs, root); got != tc.want {
				t.Errorf("classifyHeuristic(%q) = %v, want %v", tc.cmd, got, tc.want)
			}
		})
	}
}

// TestClassifyHeuristic_OpaqueArgsNeverProveContainment is THE invariant test
// of change 0070 D5, and the reason D5 exists as its own task.
//
// An opaque argument — one whose value is not statically known ($VAR, $(…)) —
// must NEVER be resolved as a filesystem path for a containment proof. Opaque
// is *unprovable ⇒ ask*, never *resolves-under-cwd ⇒ allow*.
//
// The test deliberately runs with the WORKSPACE ROOT AS THE CWD, which is what
// makes it load-bearing. withinWorkspace resolves a relative operand against
// the cwd and walks up to the deepest EXISTING ancestor, so the literal token
// "$(echo /)" resolves to <cwd>/$(echo /) — a path that does not exist, whose
// parent is the workspace, and which therefore PROVES CONTAINMENT against the
// very root the command is about to escape. That is a silent fail-OPEN: bash
// would run `rm /`. Under a scratch root the rows would pass for the wrong
// reason (out-of-root ⇒ ask), proving nothing.
//
// Two independent assertions per row, because each catches a different way to
// get this wrong:
//
//  1. pathArgs must not EMIT the opaque token (it must never reach
//     withinWorkspace/resolveExisting at all); and
//  2. the verdict must not be allow — dropping the token from pathArgs without
//     adding the ask leaves a mutating segment with ZERO path args to fail on,
//     which allows outright. Guard 1 without guard 2 is a worse bypass than
//     neither.
//
// Same failure family as the #0068 learning
// `containment-proof-needs-a-real-resolved-path` (an unexpanded ~ and a
// process-name operand both resolved against the cwd and silently proved
// in-workspace). Opaque args are the third member.
func TestClassifyHeuristic_OpaqueArgsNeverProveContainment(t *testing.T) {
	root := t.TempDir()
	canon, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	restore := chdir(t, canon)
	defer restore()

	cases := []struct {
		desc string
		cmd  string
	}{
		{"rm of a substituted root", "rm $(echo /)"},
		{"rm of a variable", "rm $VAR"},
		{"touch under a substituted home", "touch $(echo ~)/x"},
		{"mv from a variable source", "mv $SRC /tmp/x"},
		{"recursive rm of a variable", "rm -rf $HOME"},
		{"chmod of a variable target", "chmod 777 $TARGET"},
		{"a variable path joined onto a literal prefix", "rm ./$SUB"},
		{"dd output to a variable", "dd if=/dev/zero of=$OUT"},
		{"a mutating segment beside a clean read-only one", "ls -la && rm $VAR"},
		// Review finding "important 5": an ANSI-C quoted word carries UNDECODED
		// escape text in the AST, so reading it as a literal value proves
		// containment over a string the shell never resolves to. `rm -rf $'\x2f'`
		// used to resolve as <cwd>/\x2f — a leaf under the workspace — prove
		// containment, and auto-approve while bash ran `rm -rf /`.
		{"rm of an ANSI-C quoted root", `rm -rf $'\x2f'`},
		{"rm of an ANSI-C quoted home", `rm -rf $'\x7e'`},
		{"rm of an ANSI-C quoted parent escape", `rm -rf $'\x2e\x2e\x2f\x2e\x2e'`},
		{"a literal prefix joined to an ANSI-C quoted tail", `rm -rf ./$'\x2e\x2e'`},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			segs := segsFor(t, tc.cmd)

			for i, s := range segs {
				for _, a := range pathArgs(s) {
					if strings.ContainsAny(a, "$`") {
						t.Errorf("pathArgs(segment %d of %q) emitted the opaque token %q — "+
							"an opaque arg must never reach withinWorkspace/resolveExisting", i, tc.cmd, a)
					}
				}
			}

			if got := classifyHeuristic(segs, []string{canon}); got == VerdictAllow {
				t.Errorf("classifyHeuristic(%q) = VerdictAllow with the workspace as cwd — "+
					"an opaque arg is unprovable and must fail toward the human", tc.cmd)
			}
		})
	}
}

// TestClassifyHeuristic_WrapperOptionValueIsNotArgv0 is the classifier-level
// half of review finding "important 4", reproduced with the workspace as cwd —
// the setup in which the mis-peel is not merely wrong but silently ALLOWING.
//
// `nice -n 5 curl http://evil.example/x`: nice takes its adjustment as a
// separate word, so a peel that blindly drops leading `-` words leaves `5` in
// argv[0] and `curl <url>` as its arguments. Every name-keyed check downstream
// is then consulted with the wrong name — egressNames["5"] is false, so the
// egress boundary (the FIRST rule in classifyHeuristic, and the one written so
// egress "can never fall through to a silent allow") never fires — and the two
// remaining words resolve as cwd-relative paths that both prove in-workspace.
// The result was VerdictAllow on arbitrary network egress with no human.
func TestClassifyHeuristic_WrapperOptionValueIsNotArgv0(t *testing.T) {
	root := t.TempDir()
	canon, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	restore := chdir(t, canon)
	defer restore()

	for _, cmd := range []string{
		"nice -n 5 curl http://evil.example/x",
		"nice --adjustment 5 curl http://evil.example/x",
		"stdbuf -o 0 curl http://evil.example/x",
		"stdbuf --error 0 wget http://evil.example/x",
	} {
		t.Run(cmd, func(t *testing.T) {
			segs, err := splitSegments(cmd)
			if err != nil {
				return // fail-closed at the parse floor is an acceptable answer.
			}
			for i, s := range segs {
				if s.Name == "5" || s.Name == "0" {
					t.Errorf("segment %d of %q is named %q: the wrapper's option "+
						"VALUE was mistaken for argv[0]", i, cmd, s.Name)
				}
			}
			if got := classifyHeuristic(segs, []string{canon}); got == VerdictAllow {
				t.Errorf("classifyHeuristic(%q) = VerdictAllow with the workspace as "+
					"cwd — network egress must never auto-approve", cmd)
			}
		})
	}
}

// TestPathArgs_SkipsOpaquePositions asserts the index alignment directly: the
// literal operands of a mixed segment are still scoped (skipping too much would
// be its own bypass — a real out-of-root path silently unchecked), and only the
// opaque positions are dropped.
func TestPathArgs_SkipsOpaquePositions(t *testing.T) {
	segs := segsFor(t, "mv $SRC /tmp/x")
	got := pathArgs(segs[0])
	if !equalArgs(got, []string{"/tmp/x"}) {
		t.Errorf("pathArgs = %v, want [/tmp/x] (opaque $SRC dropped, literal kept)", got)
	}
	// dd's of=/if= splitting must not run on an opaque operand: splitting
	// "of=$OUT" would hand "$OUT" to the scoper as if it were a path.
	dd := segsFor(t, "dd if=/dev/zero of=$OUT")
	for _, a := range pathArgs(dd[0]) {
		if strings.Contains(a, "$") {
			t.Errorf("pathArgs(dd) = %v, must not split/scope an opaque of= operand", pathArgs(dd[0]))
		}
	}
}

// TestIsLoopbackFetch_OpaqueURLDoesNotInherit pins the #0068 loopback allow
// against D5's widening: the allow rests on PROVING every URL operand is
// loopback, and an opaque operand proves nothing.
// The workspace root is the cwd here, and that is what gives the `-o $OUT` row
// its teeth: without the guard, isLoopbackFetch reaches its scopedVal arm,
// hands the literal token "$OUT" to withinAnyRoot, resolves it to <cwd>/$OUT —
// under the root — and returns TRUE. A genuinely loopback URL would then carry
// an ARBITRARY output file through to an auto-approve. The bare `curl $URL`
// rows pass on an accident (urlHost declines to call "$URL" a loopback host)
// and prove nothing on their own.
func TestIsLoopbackFetch_OpaqueURLDoesNotInherit(t *testing.T) {
	root := t.TempDir()
	canon, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	restore := chdir(t, canon)
	defer restore()
	root = canon
	for _, cmd := range []string{
		"curl $URL",
		"curl -sS http://localhost:4000/health $URL",
		"curl -o $OUT http://localhost:4000/health",
		"curl -sS -o $OUT http://127.0.0.1:8080/x",
		"wget $URL",
	} {
		segs := segsFor(t, cmd)
		if isLoopbackFetch(segs[len(segs)-1], []string{root}) {
			t.Errorf("isLoopbackFetch(%q) = true — an opaque operand is not a proven loopback URL", cmd)
		}
		if got := classifyHeuristic(segs, []string{root}); got != VerdictAsk {
			t.Errorf("classifyHeuristic(%q) = %v, want VerdictAsk", cmd, got)
		}
	}
}

// TestClassifyHeuristic_OpaqueGitRemote pins the one place an opaque arg has to
// be read STRICTLY rather than merely dropped. isEgress decides whether a git
// remote operand names a host by inspecting the text for "://", "@" and ":"
// markers — and "$URL" carries none of them, so without the guard `git clone
// $URL` reads as a LOCAL remote name (an `origin`) and never crosses the egress
// boundary at all. Unprovable means the stricter reading, not the convenient
// one.
func TestClassifyHeuristic_OpaqueGitRemote(t *testing.T) {
	root := t.TempDir()
	canon, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	restore := chdir(t, canon)
	defer restore()
	for _, cmd := range []string{
		"git clone $URL",
		"git fetch $REMOTE",
		"git pull $REMOTE main",
	} {
		segs := segsFor(t, cmd)
		if !isEgress(segs[0]) {
			t.Errorf("isEgress(%q) = false — an opaque remote operand must be read as host-qualified", cmd)
		}
		if got := classifyHeuristic(segs, []string{canon}); got != VerdictAsk {
			t.Errorf("classifyHeuristic(%q) = %v, want VerdictAsk", cmd, got)
		}
	}
}
