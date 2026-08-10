//go:build pgstore

package main

// durable_backend_pg.go is the TAGGED durable-backend selector (change 0047), built
// only under `//go:build pgstore`. It mirrors durable_backend.go's signature so
// cmd/fuse consumes one selectDurableBackend seam regardless of build tag.
//
// For THIS cluster (Tasks 5-7) the pgstore backend (Task 8) is not yet wired: this
// file is a thin stub that returns the SAME filesystem backend as the untagged path,
// so `go build -tags pgstore ./...` compiles and behaves identically. It deliberately
// imports NO pgx here yet — the actual Postgres selection (read a DSN from cfg, call
// pgstore.Open) lands in Task 8 when internal/event/pgstore exists. Keeping this a
// non-pgx stub means neither the tagged nor the untagged build pulls pgx until the
// real pgstore backend is added.

import (
	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/event/fsstore"
	"github.com/ethanhinson/fuse/internal/session"
)

// selectDurableBackend (pgstore build) currently falls through to the filesystem
// backend. Task 8 will select pgstore when cfg names a Postgres DSN, else fall through
// here to fsstore.
func selectDurableBackend(cfg config.Config) (event.DurableStore, event.LoopRegistry, error) {
	s := fsstore.NewDurableFSStore(session.DefaultLogDir())
	return s, s, nil
}
