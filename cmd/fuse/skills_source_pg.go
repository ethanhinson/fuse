//go:build pgstore

package main

// skills_source_pg.go is the TAGGED Postgres skill-source selector (change
// 0134), built only under `//go:build pgstore`. It mirrors
// durable_backend_pg.go's DSN discipline: the DSN comes from the environment
// (FUSE_PG_DSN, else DATABASE_URL) so config.Config stays unchanged, and this
// file is the ONLY place cmd/fuse imports the skills pgsource (and thus pgx)
// besides the durable backend selector, keeping the untagged binary
// Postgres-free.

import (
	"context"
	"fmt"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/skills"
	"github.com/ethanhinson/fuse/internal/skills/pgsource"
)

// newPgSkillSource opens the Postgres skill source over the configured DSN.
// Without a DSN it is a clear configuration error: skills.source: postgres
// cannot fall back to a filesystem default.
func newPgSkillSource(cfg config.Config) (skills.Source, error) {
	dsn := pgDSN()
	if dsn == "" {
		return nil, fmt.Errorf("skills.source: postgres requires a Postgres DSN (FUSE_PG_DSN or DATABASE_URL)")
	}
	s, err := pgsource.Open(context.Background(), dsn)
	if err != nil {
		return nil, err
	}
	return s, nil
}