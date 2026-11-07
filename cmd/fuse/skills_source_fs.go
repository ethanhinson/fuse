//go:build !pgstore

package main

// skills_source_fs.go is the UNTAGGED skill-source selector (change 0134): an
// untagged binary has no Postgres skill source compiled in, so
// skills.source: postgres is a clear error here. The pgstore build overrides
// it with skills_source_pg.go (same signature).

import (
	"fmt"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/skills"
)

func newPgSkillSource(cfg config.Config) (skills.Source, error) {
	return nil, fmt.Errorf("skills.source: postgres is not compiled into this binary (build with -tags pgstore)")
}