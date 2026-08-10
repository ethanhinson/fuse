//go:build pgstore

package main

// durable_backend_pg.go is the TAGGED durable-backend selector (change 0047), built
// only under `//go:build pgstore`. It mirrors durable_backend.go's signature so
// cmd/fuse consumes one selectDurableBackend seam regardless of build tag.
//
// When a Postgres DSN is configured it selects the pgstore backend (the deployable,
// cross-instance one). With no DSN it falls through to the filesystem backend, so a
// `-tags pgstore` binary still works locally without a database. The DSN is read
// from the environment (FUSE_PG_DSN, else DATABASE_URL) so config.Config stays
// unchanged — the tagged path never mutates the shared schema. This file is the ONLY
// place cmd/fuse imports pgstore (and thus pgx), keeping the untagged binary
// Postgres-free (ADR-0030 composition-root selection).
//
// pgstore.Open is a value returned into runtime.Deps (never a construction-time
// shared global): one *pgstore.PGStore satisfies BOTH event.DurableStore and
// event.LoopRegistry, so it is returned as both seams.

import (
	"context"
	"os"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/event/fsstore"
	"github.com/ethanhinson/fuse/internal/event/pgstore"
	"github.com/ethanhinson/fuse/internal/session"
)

// pgDSN returns the configured Postgres DSN (FUSE_PG_DSN, else DATABASE_URL), or ""
// when none is set.
func pgDSN() string {
	if dsn := os.Getenv("FUSE_PG_DSN"); dsn != "" {
		return dsn
	}
	return os.Getenv("DATABASE_URL")
}

// selectDurableBackend (pgstore build) selects the Postgres backend when a DSN is
// configured, else falls through to the filesystem backend. The pgstore store IS its
// own registry (PGStore satisfies both interfaces), so the same value is returned for
// both seams — a fresh process (or another instance) resolves a prior loop from the
// shared database, giving cross-instance cold reattach + a shared live tail.
func selectDurableBackend(cfg config.Config) (event.DurableStore, event.LoopRegistry, error) {
	if dsn := pgDSN(); dsn != "" {
		s, err := pgstore.Open(context.Background(), dsn)
		if err != nil {
			return nil, nil, err
		}
		return s, s, nil
	}
	s := fsstore.NewDurableFSStore(session.DefaultLogDir())
	return s, s, nil
}
