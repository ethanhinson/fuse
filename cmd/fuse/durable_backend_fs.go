package main

import (
	"fmt"
	"os"

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
		return nil, fmt.Errorf("durable store: create %s: %w", dir, err)
	}
	return fsstore.NewDurableFSStore(dir), nil
}
