//go:build pgstore

package pgsource

// conformance_test.go runs the skills.Source conformance suite against the
// Postgres source (change 0134). It runs only under `//go:build pgstore` and
// is skip-clean: without a container runtime (Docker) it t.Skips, unless
// FUSE_TEST_PG_DSN names a server to use — the same harness contract as
// internal/artifact/pgstore.

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/ethanhinson/fuse/internal/skills"
	"github.com/ethanhinson/fuse/internal/skills/skillstest"
)

var (
	pgHarnessOnce sync.Once
	pgHarnessDSN  string
	pgSkipReason  string
)

func getPGHarness() (string, string) {
	pgHarnessOnce.Do(func() {
		if dsn := getenvFUSE_TEST_PG_DSN(); dsn != "" {
			pgHarnessDSN = dsn
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		container, err := tcpostgres.Run(ctx,
			"postgres:16-alpine",
			tcpostgres.WithDatabase("fuse_test"),
			tcpostgres.WithUsername("fuse"),
			tcpostgres.WithPassword("fuse"),
			tcpostgres.BasicWaitStrategies(),
		)
		if err != nil {
			pgSkipReason = "no container runtime: " + err.Error()
			return
		}
		dsn, err := container.ConnectionString(ctx, "sslmode=disable")
		if err != nil {
			pgSkipReason = "connection string: " + err.Error()
			return
		}
		pgHarnessDSN = dsn
	})
	return pgHarnessDSN, pgSkipReason
}

func getenvFUSE_TEST_PG_DSN() string {
	// the same env var the artifact pgstore harness reads, so FUSE_TEST_PG_DSN
	// names one server for every tagged Postgres suite.
	return os.Getenv("FUSE_TEST_PG_DSN")
}

// pgSourceFactory wipes the skills table, seeds it, and returns a fresh
// pgsource Store over the shared harness DSN.
func pgSourceFactory() func(t *testing.T, seed []skills.Skill) (skills.Source, func()) {
	dsn, skip := getPGHarness()
	return func(t *testing.T, seed []skills.Skill) (skills.Source, func()) {
		if skip != "" {
			t.Skipf("skills pgsource: %s", skip)
		}
		s, err := Open(context.Background(), dsn)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if _, err := s.pool.Exec(context.Background(), `DELETE FROM skills`); err != nil {
			t.Fatalf("wipe: %v", err)
		}
		for _, sk := range seed {
			if err := s.Upsert(context.Background(), sk.Name, sk.Description, sk.Body); err != nil {
				t.Fatalf("seed %s: %v", sk.Name, err)
			}
		}
		return s, func() { s.Close() }
	}
}

// TestPGSkillSourceConformance runs the full source contract against the
// Postgres source.
func TestPGSkillSourceConformance(t *testing.T) {
	skillstest.RunSourceConformance(t, pgSourceFactory())
}