package permissions

import (
	"os"
	"path/filepath"
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
