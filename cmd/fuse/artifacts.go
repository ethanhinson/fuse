package main

import (
	"fmt"
	"os"
	"path/filepath"

	artfs "github.com/ethanhinson/fuse/internal/artifact/fsstore"
	"github.com/ethanhinson/fuse/internal/session"
)

// artifactsDir is the filesystem artifact store's root, ~/.fuse/artifacts: a sibling
// of the sessions directory, shared by every local binding.
func artifactsDir() string {
	return filepath.Join(filepath.Dir(session.DefaultLogDir()), "artifacts")
}

// newFSArtifactStore opens the filesystem artifact store under artifactsDir(),
// creating the directory up front so an unwritable home fails at startup rather than
// on a loop's first spill.
func newFSArtifactStore() (*artfs.Store, error) {
	dir := artifactsDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("filesystem artifact store: create %s: %w", dir, err)
	}
	return artfs.New(dir), nil
}
