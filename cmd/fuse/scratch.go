package main

import (
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/session"
)

// scratchMaxAge is how long an abandoned session scratch directory survives
// before the startup sweep removes it (mirrors the session-log sweep window).
const scratchMaxAge = 7 * 24 * time.Hour

var (
	scratchOnce sync.Once
	scratchPath string
)

// sessionScratchDir lazily creates this process's scratch directory under
// ~/.fuse/tmp/ (change 0068): a per-session workspace-equivalent area the
// auto-mode path scoping auto-approves, advertised in the system prompt so
// models use it instead of /tmp. The path is canonicalized (EvalSymlinks) so
// containment checks match — on macOS an uncanonicalized temp path never
// matches its /private-resolved form. Returns "" when creation fails: the gate
// simply gets no extra root and behavior degrades to workspace-only scoping.
func sessionScratchDir() string {
	scratchOnce.Do(func() {
		scratchPath = createScratchDir(filepath.Join(filepath.Dir(session.DefaultLogDir()), "tmp"))
	})
	return scratchPath
}

// createScratchDir makes one canonicalized scratch directory under base,
// sweeping stale siblings first. Returns "" on any failure.
func createScratchDir(base string) string {
	if err := os.MkdirAll(base, 0o700); err != nil {
		return ""
	}
	sweepScratch(base)
	dir, err := os.MkdirTemp(base, "session-")
	if err != nil {
		return ""
	}
	if canon, err := filepath.EvalSymlinks(dir); err == nil {
		dir = canon
	}
	return dir
}

// sweepScratch removes session scratch directories older than scratchMaxAge.
// Best-effort: a failed removal is skipped, never fatal.
func sweepScratch(base string) {
	entries, err := os.ReadDir(base)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-scratchMaxAge)
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		_ = os.RemoveAll(filepath.Join(base, e.Name()))
	}
}

// gateWriteRoots resolves the extra write roots for a gate (change 0068): the
// per-session scratch dir plus each trusted permissions.auto.write_roots entry,
// every one canonicalized. A config root that cannot be resolved (missing dir,
// symlink error) is skipped — an unresolvable root can never scope safely.
func gateWriteRoots(cfg config.Config) []string {
	var roots []string
	if s := sessionScratchDir(); s != "" {
		roots = append(roots, s)
	}
	for _, r := range cfg.Permissions.Auto.WriteRoots {
		if canon, err := filepath.EvalSymlinks(r); err == nil {
			roots = append(roots, canon)
		}
	}
	return roots
}

// appendScratchBlock appends the scratch-directory advertisement to a system
// prompt's extra block so models reach for the auto-approved scratch area
// instead of /tmp.
func appendScratchBlock(extra string) string {
	s := sessionScratchDir()
	if s == "" {
		return extra
	}
	block := "Scratch directory: " + s + " — use it for temporary files instead of /tmp; writes there are auto-approved in auto mode."
	if extra == "" {
		return block
	}
	return extra + "\n\n" + block
}
