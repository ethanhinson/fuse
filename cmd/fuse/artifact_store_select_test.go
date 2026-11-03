//go:build !pgstore

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/artifact"
	artfs "github.com/ethanhinson/fuse/internal/artifact/fsstore"
	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/event"
)

// TestSelectArtifactStoreUntaggedIsFS: the untagged composition root always
// selects the filesystem artifact store rooted at ~/.fuse/artifacts, and creates
// that directory up front so a fresh host fails at startup, not on a first spill.
func TestSelectArtifactStoreUntaggedIsFS(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	s, err := selectArtifactStore(config.Config{})
	if err != nil {
		t.Fatalf("selectArtifactStore: %v", err)
	}
	if _, ok := s.(*artfs.Store); !ok {
		t.Fatalf("selectArtifactStore = %T, want *fsstore.Store", s)
	}
	dir := filepath.Join(home, ".fuse", "artifacts")
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("%s not created: %v", dir, err)
	}
	b := artifact.Binding{Store: s, Tenant: event.DefaultTenant, Loop: "loop1"}
	if _, err := b.Put(context.Background(), artifact.KindSpill, strings.NewReader("x"), artifact.Meta{}); err != nil {
		t.Fatalf("put: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, string(event.DefaultTenant), "loop1")); err != nil {
		t.Fatalf("artifact not stored under %s: %v", dir, err)
	}
}
