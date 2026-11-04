package main

import (
	"fmt"
	"os"
	"path/filepath"

	artfs "github.com/ethanhinson/fuse/internal/artifact/fsstore"
	"github.com/ethanhinson/fuse/internal/event/fsstore"
	"github.com/ethanhinson/fuse/internal/session"
)

// newFSDurableBackend opens the filesystem durable store rooted at
// session.DefaultLogDir(), creating that directory up front. The store itself only
// creates it on a loop's first event, but /readyz pings it, so without this a fresh
// host reported not-ready until a first loop ran - which a load balancer that waits
// for ready never lets happen. A directory that cannot be created now fails startup
// instead of every later loop.
func newFSDurableBackend() (*fsstore.FSDurableStore, error) {
	dir := session.DefaultLogDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fsBackendError{fmt.Errorf("filesystem durable store: create %s: %w", dir, err)}
	}
	return fsstore.NewDurableFSStore(dir), nil
}

// artifactsDir is the filesystem artifact store's root, ~/.fuse/artifacts: a
// sibling of the sessions directory, shared by every local binding.
func artifactsDir() string {
	return filepath.Join(filepath.Dir(session.DefaultLogDir()), "artifacts")
}

// newFSArtifactStore opens the filesystem artifact store under artifactsDir(),
// creating the directory up front so an unwritable home fails at startup rather
// than on a loop's first spill.
func newFSArtifactStore() (*artfs.Store, error) {
	dir := artifactsDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fsBackendError{fmt.Errorf("filesystem artifact store: create %s: %w", dir, err)}
	}
	return artfs.New(dir), nil
}

// fsBackendError marks a failure of a filesystem store itself (durable or
// artifact), so the startup error names its real remedy instead of the Postgres one.
type fsBackendError struct{ error }

func (e fsBackendError) Unwrap() error { return e.error }
