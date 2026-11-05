package main

import (
	"path/filepath"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/permissions"
)

// gateWriteRoots resolves the extra write roots for a gate (change 0068): each
// trusted permissions.auto.write_roots entry, canonicalized. A config root
// that cannot be resolved (missing dir, symlink error) is skipped — an
// unresolvable root can never scope safely. The scratch area is now
// scratch/ under the workspace root (change 0129), which the gate already
// scopes via WithWorkspaceRoot — no separate process-global root is needed.
func gateWriteRoots(cfg config.Config) []string {
	var roots []string
	for _, r := range cfg.Permissions.Auto.WriteRoots {
		if canon, err := filepath.EvalSymlinks(r); err == nil {
			roots = append(roots, canon)
		}
	}
	return roots
}

// scratchBlockWanted reports whether the prompt should advertise the scratch
// directory at all. Its point is "writes here need no approval", which only
// means something while a gate can ask; with permissions off (including
// --approve-all) every path is equally free and the advertisement only pulls
// the model's files away from the working directory.
func scratchBlockWanted(cfg config.Config) bool {
	return permissions.ParseMode(cfg.Permissions.Mode) != permissions.ModeOff
}

// appendScratchBlock appends the scratch-directory advertisement to a system
// prompt's extra block so models reach for the auto-approved scratch area
// instead of /tmp.
//
// The wording draws the line explicitly: once the prompt shed its skills and
// spawn blocks (2026-09-22) this sentence became the most prominent
// instruction in it, and a plain "use it for temporary files" sent models to
// write the task's own deliverable there (LCB-100: solution.py landed in the
// scratch directory on 34% of problems, against 1% before), so the run ended
// with the workspace file empty.
//
// Change 0129: the advertised path is now "scratch/", workspace-relative. The
// file tools and bash both operate inside the workspace, so a relative path is
// the honest path — no absolute host path leaks into the system prompt (a
// leak that was also a cross-tenant read path on the server bindings). The
// process-global scratch dir is retired; the workspace's own scratch/ (created
// at Open) is the scratch area.
func appendScratchBlock(extra string) string {
	block := "Scratch directory for throwaway files only (probes, drafts, temporary output), auto-approved in auto mode: scratch/." +
		" Files the task asks for belong in the working directory, never there."
	if extra == "" {
		return block
	}
	return extra + "\n\n" + block
}
