package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/config"
)

// TestCreateScratchDir proves the change-0068 scratch creation: one
// canonicalized directory under the given base. (sessionScratchDir itself is a
// sync.Once over this against the real home — not assertable in a shared test
// process where earlier tests ran under a since-deleted temp HOME.)
func TestCreateScratchDir(t *testing.T) {
	base := t.TempDir()
	dir := createScratchDir(base)
	if dir == "" {
		t.Fatal("createScratchDir returned empty")
	}
	if canon, err := filepath.EvalSymlinks(dir); err != nil || canon != dir {
		t.Errorf("scratch dir not canonicalized: %q vs %q (%v)", dir, canon, err)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatalf("scratch dir not created: %v", err)
	}
}

// TestScratchAdvertisedAndInRoots proves the process-level surface is stable
// and consistent: whatever sessionScratchDir resolved to, gateWriteRoots leads
// with it and appendScratchBlock advertises it.
func TestScratchAdvertisedAndInRoots(t *testing.T) {
	dir := sessionScratchDir()
	if again := sessionScratchDir(); again != dir {
		t.Errorf("scratch dir not stable across calls: %q vs %q", again, dir)
	}
	if dir == "" {
		t.Skip("scratch creation unavailable in this environment")
	}
	roots := gateWriteRoots(config.Config{})
	if len(roots) == 0 || roots[0] != dir {
		t.Errorf("gateWriteRoots = %v, want scratch dir first", roots)
	}
	extra := appendScratchBlock("existing block")
	if !strings.Contains(extra, dir) || !strings.Contains(extra, "existing block") {
		t.Errorf("appendScratchBlock lost content: %q", extra)
	}
}

// TestGateWriteRoots_ConfigRootsCanonicalized proves config write_roots are
// canonicalized and unresolvable roots are skipped.
func TestGateWriteRoots_ConfigRootsCanonicalized(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{}
	cfg.Permissions.Auto.WriteRoots = []string{link, "/does/not/exist"}
	roots := gateWriteRoots(cfg)
	canonReal, _ := filepath.EvalSymlinks(real)
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

// TestSweepScratch removes only stale entries.
func TestSweepScratch(t *testing.T) {
	base := t.TempDir()
	old := filepath.Join(base, "session-old")
	fresh := filepath.Join(base, "session-fresh")
	for _, d := range []string{old, fresh} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	stale := time.Now().Add(-scratchMaxAge - time.Hour)
	if err := os.Chtimes(old, stale, stale); err != nil {
		t.Fatal(err)
	}
	sweepScratch(base)
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("stale scratch dir survived the sweep")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("fresh scratch dir was swept")
	}
}
