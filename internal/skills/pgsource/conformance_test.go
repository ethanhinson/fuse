//go:build pgstore

package pgsource

// conformance_test.go runs the skills.Source conformance suite against the
// Postgres source (change 0134). It runs only under `//go:build pgstore` and
// is skip-clean: without a container runtime (Docker) it t.Skips, unless
// FUSE_TEST_PG_DSN names a server to use — the same harness contract as
// internal/artifact/pgstore.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

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

// pgSchemaSeq names a fresh schema per factory call, so each source the
// conformance suite builds is backed by its own table (the contract says each
// call produces an independent source) rather than one shared, wiped one.
var pgSchemaSeq atomic.Uint64

// pgSourceFactory creates a new schema with a unique name from the counter,
// seeds it, and returns a fresh pgsource Store whose search_path is that
// schema. Seeds are first-wins: a repeated name is skipped, because the
// schema's skills table holds one row per name.
func pgSourceFactory() func(t *testing.T, seed []skills.Skill) (skills.Source, func()) {
	dsn, skip := getPGHarness()
	return func(t *testing.T, seed []skills.Skill) (skills.Source, func()) {
		if skip != "" {
			t.Skipf("skills pgsource: %s", skip)
		}
		schema := fmt.Sprintf("skills_conf_%d", pgSchemaSeq.Add(1))
		ctx := context.Background()
		// A short-lived connection creates the schema; the Store's own pool
		// then never needs DDL rights beyond Open's idempotent apply.
		conn, err := pgx.Connect(ctx, dsn)
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		if _, err := conn.Exec(ctx, fmt.Sprintf("CREATE SCHEMA %s", pgx.Identifier{schema}.Sanitize())); err != nil {
			_ = conn.Close(context.Background())
			t.Fatalf("create schema: %v", err)
		}
		if err := conn.Close(ctx); err != nil {
			t.Fatalf("close schema conn: %v", err)
		}
		// Open the Store on the DSN with search_path set to the schema, joining
		// with ? or & as the DSN requires.
		sep := "?"
		if strings.Contains(dsn, "?") {
			sep = "&"
		}
		s, err := Open(ctx, dsn+sep+"search_path="+schema)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		seen := map[string]bool{}
		for _, sk := range seed {
			if seen[sk.Name] {
				continue // a table holds one row per name; the first seed wins
			}
			seen[sk.Name] = true
			if err := s.Upsert(ctx, sk.Name, sk.Description, sk.Body); err != nil {
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
