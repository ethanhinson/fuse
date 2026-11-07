package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// agentsMD.go composes the project-instructions block for the root agent's
// system prompt. The block is built ONCE at startup (one-shot run or shell
// session) and appended to the root's instructions, so the composed prompt
// prefix is stable across turns and spawned children — which never receive
// cfg.Instructions — never see it.

// agentsMDMaxBytes caps the total project-instructions payload the prompt
// carries. Past it the last included file is truncated with a visible note and
// one warning goes to stderr.
const agentsMDMaxBytes = 32 * 1024

// agentsMDHeader introduces the wrapped files so the model knows what they are.
const agentsMDHeader = "The following are project-specific instructions and guidelines:"

// agentsMDTruncatedNote is the visible marker appended when the size limit cut
// the last included file short.
const agentsMDTruncatedNote = "\n[... instructions truncated: AGENTS.md content exceeded 32KB total ...]"

// agentsMDInstructions reads AGENTS.md files at startup and returns the
// project-instructions block to append to the root agent's instructions, or ""
// when no file yields content — in which case the composed system prompt is
// byte-identical to one built without this feature. Order: ~/.fuse/AGENTS.md
// first, then every AGENTS.md from the repository root (the nearest ancestor
// with a .git entry, which may be a FILE in a linked worktree) down to cwd.
// With no repository only cwd's file is read, and the walk never goes above
// the repository root.
//
// A missing, empty or unreadable file changes nothing. A repository file that
// resolves (through symlinks) outside the repository is skipped with a
// warning, since a repository must not be able to pull arbitrary files into
// the prompt; the home file is trusted and exempt from that check. warn may be
// nil (tests); stderrFprintf(warn, ...) is used otherwise.
func agentsMDInstructions(cwd, home string, warn io.Writer) string {
	cwdAbs, err := filepath.Abs(cwd)
	if err != nil {
		cwdAbs = cwd
	}
	repoRoot := findRepoRoot(cwdAbs)

	var paths []string
	if home != "" {
		if hp := filepath.Join(home, ".fuse", "AGENTS.md"); isNonEmptyFile(hp) {
			paths = append(paths, hp)
		}
	}
	// Walk cwd up to (and including) the repository root — or only cwd itself
	// when there is no repository — collecting AGENTS.md files leaf-first, then
	// reverse so the root's file precedes the working directory's.
	var chain []string
	for dir := cwdAbs; ; dir = filepath.Dir(dir) {
		if p := filepath.Join(dir, "AGENTS.md"); isNonEmptyFile(p) {
			chain = append(chain, p)
		}
		if repoRoot == "" || dir == repoRoot {
			break
		}
	}
	for i := len(chain) - 1; i >= 0; i-- {
		paths = append(paths, chain[i])
	}
	if len(paths) == 0 {
		return ""
	}

	repoBoundary := repoRoot
	if repoBoundary == "" {
		// No repository: the working directory is the boundary a file read
		// through a symlink must stay within.
		repoBoundary = cwdAbs
	}
	// Compare fully-resolved paths on both sides: EvalSymlinks on the file also
	// resolves symlinked ancestors (e.g. a /tmp symlink), so the boundary must
	// be resolved the same way or every file "escapes".
	if rb, rerr := filepath.EvalSymlinks(repoBoundary); rerr == nil {
		repoBoundary = rb
	}

	var b strings.Builder
	total := 0
	truncated := false
	for _, p := range paths {
		if truncated {
			break
		}
		// The home file is trusted; every other file must resolve inside the
		// boundary so a repository cannot smuggle outside content into the
		// prompt via a symlink.
		if home == "" || p != paths[0] || !strings.HasPrefix(p, filepath.Join(home, ".fuse")) {
			resolved, rerr := filepath.EvalSymlinks(p)
			if rerr != nil || !withinDir(repoBoundary, resolved) {
				stderrFprintf(warn, "AGENTS.md: skipping %s: resolves outside the repository\n", p)
				continue
			}
		}
		// One byte past what fits is enough to see the limit was passed, and a
		// repository's huge file costs no more memory than that.
		data, rerr := readAtMost(p, agentsMDMaxBytes-total+1)
		if rerr != nil || len(strings.TrimSpace(string(data))) == 0 {
			continue // unreadable or empty changes nothing
		}
		content := string(data)
		if total+len(content) > agentsMDMaxBytes {
			// A cut can land inside a multi-byte character.
			content = strings.ToValidUTF8(content[:agentsMDMaxBytes-total], "") + agentsMDTruncatedNote
			truncated = true
			stderrFprintf(warn, "AGENTS.md: instructions exceeded %dKB total; truncated\n", agentsMDMaxBytes/1024)
		}
		total += len(content)
		fmt.Fprintf(&b, "\n\n<project_instructions path=%q>\n%s\n</project_instructions>", p, content)
	}
	if b.Len() == 0 {
		return "" // every file was skipped: the prompt changes nothing
	}
	return agentsMDHeader + b.String()
}

// readAtMost reads up to n bytes of path.
func readAtMost(path string, n int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, int64(n)))
}

// stderrFprintf prints a warning when warn is non-nil and no-ops otherwise, so
// unit tests can pass nil.
func stderrFprintf(warn io.Writer, format string, args ...any) {
	if warn != nil {
		fmt.Fprintf(warn, format, args...)
	}
}

// isNonEmptyFile reports whether path is a readable regular file with at least
// one byte (cheap existence pre-check before the real read).
func isNonEmptyFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Size() > 0
}

// findRepoRoot returns the nearest ancestor of dir (including dir) containing
// a .git entry — a directory for a normal checkout, a FILE for a linked
// worktree — or "" when there is no repository. It never returns anything
// above the first match.
func findRepoRoot(dir string) string {
	for {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// withinDir reports whether path lies inside root (or equals it), after both
// are cleaned. Boundary-safe: /repo-evil is not within /repo.
func withinDir(root, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
