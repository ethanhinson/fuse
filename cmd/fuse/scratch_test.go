package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
)

// TestScratchAdvertisedRelative proves the change-0129 scratch advertisement
// is workspace-relative ("scratch/") and never an absolute host path, and that
// it still draws the scratch/deliverable line. The process-global scratch dir
// is retired (criterion 6: grep -rn sessionScratchDir is empty).
func TestScratchAdvertisedRelative(t *testing.T) {
	extra := appendScratchBlock("existing block")
	if !strings.Contains(extra, "existing block") {
		t.Errorf("appendScratchBlock lost existing content: %q", extra)
	}
	if !strings.Contains(extra, "scratch/") {
		t.Errorf("appendScratchBlock does not advertise scratch/: %q", extra)
	}
	// No absolute host path — the advertised path is relative.
	if strings.Contains(extra, ":/") || filepath.IsAbs(strings.TrimSpace(strings.Split(extra, ": ")[len(strings.Split(extra, ": "))-1])) {
		// Heuristic: the block should not contain a Windows drive or a Unix
		// absolute path in the advertisement. Check the block does not name
		// a path starting with "/" or "~".
		if strings.Contains(extra, ": /") || strings.Contains(extra, "~/") {
			t.Errorf("appendScratchBlock advertises an absolute/host path: %q", extra)
		}
	}
	// The block must say where deliverables go, not only where scratch is.
	if !strings.Contains(extra, "working directory") || !strings.Contains(extra, "throwaway") {
		t.Errorf("appendScratchBlock does not draw the scratch/deliverable line: %q", extra)
	}
}

// TestGateWriteRoots_ConfigRootsCanonicalized proves config write_roots are
// canonicalized and unresolvable roots are skipped. The process-global scratch
// dir is no longer a write root (change 0129: scratch/ is under the workspace
// root, which the gate already scopes via WithWorkspaceRoot).
func TestGateWriteRoots_ConfigRootsCanonicalized(t *testing.T) {
	real2 := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real2, link); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{}
	cfg.Permissions.Auto.WriteRoots = []string{link, "/does/not/exist"}
	roots := gateWriteRoots(cfg)
	canonReal, _ := filepath.EvalSymlinks(real2)
	found := false
	for _, r := range roots {
		if r == canonReal {
			found = true
		}
		if r == link || r == "/does/not/exist" {
			t.Errorf("uncanonicalized/unresolvable root leaked: %v", roots)
		}
	}
	if !found {
		t.Errorf("canonicalized config root missing from %v", roots)
	}
}

// TestScratchBlockWantedFollowsMode: the advertisement rides in modes where a
// gate can ask (its "auto-approved here" claim is the point) and is dropped
// when permissions are off, which --approve-all now sets for its run.
func TestScratchBlockWantedFollowsMode(t *testing.T) {
	var cfg config.Config
	for _, mode := range []string{"auto", "smart", "prompt-all", ""} {
		cfg.Permissions.Mode = mode
		if !scratchBlockWanted(cfg) {
			t.Errorf("mode %q: scratch block should be advertised", mode)
		}
	}
	cfg.Permissions.Mode = "off"
	if scratchBlockWanted(cfg) {
		t.Errorf("mode off: scratch block must not be advertised")
	}
}