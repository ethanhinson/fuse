package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// writeFile writes content, creating parent directories.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestAgentsMDOrder covers home + repository root + subdirectory ordering and
// the wrapping format.
func TestAgentsMDOrder(t *testing.T) {
	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	repo := filepath.Join(tmp, "repo")
	sub := filepath.Join(repo, "a", "b")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(home, ".fuse", "AGENTS.md"), "HOME-RULES")
	writeFile(t, filepath.Join(repo, "AGENTS.md"), "ROOT-RULES")
	writeFile(t, filepath.Join(repo, "a", "AGENTS.md"), "MID-RULES")
	writeFile(t, filepath.Join(sub, "AGENTS.md"), "LEAF-RULES")

	got := agentsMDInstructions(sub, home, nil)
	if !strings.HasPrefix(got, agentsMDHeader) {
		t.Fatalf("missing header:\n%s", got)
	}
	want := []string{
		`<project_instructions path="` + filepath.Join(home, ".fuse", "AGENTS.md") + `">` + "\nHOME-RULES",
		`<project_instructions path="` + filepath.Join(repo, "AGENTS.md") + `">` + "\nROOT-RULES",
		`<project_instructions path="` + filepath.Join(repo, "a", "AGENTS.md") + `">` + "\nMID-RULES",
		`<project_instructions path="` + filepath.Join(sub, "AGENTS.md") + `">` + "\nLEAF-RULES",
	}
	last := -1
	for _, w := range want {
		i := strings.Index(got, w)
		if i < 0 {
			t.Fatalf("missing wrapped block %q in:\n%s", w, got)
		}
		if i < last {
			t.Fatalf("block %q out of order in:\n%s", w, got)
		}
		last = i
	}
}

// TestAgentsMDStopsAtRepoRoot: a file above the repository root is never read.
func TestAgentsMDStopsAtRepoRoot(t *testing.T) {
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	sub := filepath.Join(repo, "sub")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(tmp, "AGENTS.md"), "ABOVE-ROOT")
	writeFile(t, filepath.Join(sub, "AGENTS.md"), "LEAF")

	got := agentsMDInstructions(sub, "", nil)
	if strings.Contains(got, "ABOVE-ROOT") {
		t.Fatalf("walked above the repository root:\n%s", got)
	}
	if !strings.Contains(got, "LEAF") {
		t.Fatalf("missing leaf file:\n%s", got)
	}
}

// TestAgentsMDGitFile: a worktree's .git FILE still marks the repository root.
func TestAgentsMDGitFile(t *testing.T) {
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "worktree")
	sub := filepath.Join(repo, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	// .git as a file, exactly as git worktree creates it.
	writeFile(t, filepath.Join(repo, ".git"), "gitdir: /elsewhere\n")
	writeFile(t, filepath.Join(tmp, "AGENTS.md"), "ABOVE-ROOT")
	writeFile(t, filepath.Join(repo, "AGENTS.md"), "ROOT")
	writeFile(t, filepath.Join(sub, "AGENTS.md"), "LEAF")

	got := agentsMDInstructions(sub, "", nil)
	if strings.Contains(got, "ABOVE-ROOT") {
		t.Fatalf("walked above a .git-file worktree root:\n%s", got)
	}
	if !strings.Contains(got, "ROOT") || !strings.Contains(got, "LEAF") {
		t.Fatalf("missing worktree files:\n%s", got)
	}
}

// TestAgentsMDNoRepository: with no .git anywhere, only the working
// directory's file is read.
func TestAgentsMDNoRepository(t *testing.T) {
	tmp := t.TempDir()
	sub := filepath.Join(tmp, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(tmp, "AGENTS.md"), "ANCESTOR")
	writeFile(t, filepath.Join(sub, "AGENTS.md"), "CWD")

	got := agentsMDInstructions(sub, "", nil)
	if strings.Contains(got, "ANCESTOR") {
		t.Fatalf("read above the working directory with no repository:\n%s", got)
	}
	if !strings.Contains(got, "CWD") {
		t.Fatalf("missing working-directory file:\n%s", got)
	}
}

// TestAgentsMDEmptyAndMissing: no file, or only empty files, changes nothing —
// the block is empty so the system prompt stays byte-identical to today's.
func TestAgentsMDEmptyAndMissing(t *testing.T) {
	tmp := t.TempDir()
	if got := agentsMDInstructions(tmp, "", nil); got != "" {
		t.Fatalf("no files, got %q", got)
	}
	writeFile(t, filepath.Join(tmp, "AGENTS.md"), "  \n")
	if got := agentsMDInstructions(tmp, "", nil); got != "" {
		t.Fatalf("empty file, got %q", got)
	}
	// An unreadable file changes nothing. (Root ignores permission bits, so
	// only assert this where the mode actually bites.)
	writeFile(t, filepath.Join(tmp, "AGENTS.md"), "SECRET")
	if err := os.Chmod(filepath.Join(tmp, "AGENTS.md"), 0o000); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() != 0 {
		if got := agentsMDInstructions(tmp, "", nil); got != "" {
			t.Fatalf("unreadable file, got %q", got)
		}
	}
}

// TestAgentsMDSizeLimit: past 32KB total the last file is truncated, the
// visible note rides in the block, and exactly one warning goes to stderr.
func TestAgentsMDSizeLimit(t *testing.T) {
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	sub := filepath.Join(repo, "sub")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(repo, "AGENTS.md"), strings.Repeat("A", agentsMDMaxBytes-100))
	writeFile(t, filepath.Join(sub, "AGENTS.md"), strings.Repeat("B", 1000))

	var warn bytes.Buffer
	got := agentsMDInstructions(sub, "", &warn)
	if !strings.Contains(got, agentsMDTruncatedNote) {
		t.Fatalf("missing truncation note:\n%s", got[len(got)-500:])
	}
	if strings.Count(warn.String(), "truncated") != 1 {
		t.Fatalf("want exactly one truncation warning, got %q", warn.String())
	}
	// Only 100 B-bytes fit before the note.
	if strings.Contains(got, strings.Repeat("B", 200)) {
		t.Fatalf("truncation did not cut the last file")
	}
}

// TestAgentsMDSymlinkEscape: a repository file resolving outside the
// repository is skipped with a warning.
func TestAgentsMDSymlinkEscape(t *testing.T) {
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	outside := filepath.Join(tmp, "outside.md")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, outside, "ESCAPE-HATCH")
	if err := os.Symlink(outside, filepath.Join(repo, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}

	var warn bytes.Buffer
	got := agentsMDInstructions(repo, "", &warn)
	if strings.Contains(got, "ESCAPE-HATCH") {
		t.Fatalf("symlink escaping the repository was read:\n%s", got)
	}
	if !strings.Contains(warn.String(), "outside the repository") {
		t.Fatalf("missing escape warning, got %q", warn.String())
	}
}

// A cut that lands inside a multi-byte character must not put invalid text in the prompt.
func TestAgentsMDTruncationKeepsValidUTF8(t *testing.T) {
	cwd := t.TempDir()
	body := strings.Repeat("x", agentsMDMaxBytes-1) + "é" + strings.Repeat("y", 100)
	if err := os.WriteFile(filepath.Join(cwd, "AGENTS.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got := agentsMDInstructions(cwd, "", nil)
	if !utf8.ValidString(got) {
		t.Fatal("truncated instructions are not valid UTF-8")
	}
	if !strings.Contains(got, "instructions truncated") {
		t.Fatal("no truncation note")
	}
}
