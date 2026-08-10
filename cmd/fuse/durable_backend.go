//go:build !pgstore

package main

// durable_backend.go is the UNTAGGED durable-backend selector (change 0047). It is
// the local/dev default: the untagged `fuse` binary always wires the filesystem
// backend and NEVER imports pgx/testcontainers (the non-negotiable no-Postgres-import
// constraint — `go list -deps ./cmd/... ./internal/...` must be pgx-free without the
// `pgstore` tag). The Postgres selector lives in durable_backend_pg.go behind
// `//go:build pgstore`; the two files are mutually exclusive by build tag and both
// define selectDurableBackend with the same signature, so cmd/fuse consumes one seam
// regardless of how the binary was built.

import (
	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/event/fsstore"
	"github.com/ethanhinson/fuse/internal/session"
)

// selectDurableBackend returns the process-shared durable event store + loop registry
// the loop-server threads into runtime.Deps as VALUES (ADR-0030 policy-free seam: the
// backend is chosen HERE at the composition root, never inside internal/runtime). In
// the untagged build it is always the filesystem backend rooted at
// session.DefaultLogDir(): one *fsstore.FSDurableStore that satisfies BOTH
// event.DurableStore and event.LoopRegistry, so it is returned as both — a fresh
// process resolves a prior process's loop from the on-disk registry sidecars, giving
// `fuse loop-server` cold cross-process reattach.
//
// cfg is accepted for signature parity with the pgstore selector (which reads a
// Postgres DSN from it); the untagged path ignores it.
func selectDurableBackend(cfg config.Config) (event.DurableStore, event.LoopRegistry, error) {
	s := fsstore.NewDurableFSStore(session.DefaultLogDir())
	// The store IS its own registry (FSDurableStore satisfies both interfaces): return
	// the same value for both seams so they share one on-disk tree.
	return s, s, nil
}
