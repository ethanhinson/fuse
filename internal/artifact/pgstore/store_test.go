//go:build pgstore

package pgstore

// store_test.go is the tagged Postgres test suite for the artifact store. It
// runs only under `//go:build pgstore` and is skip-clean: without a container
// runtime (Docker) it t.Skips, unless FUSE_TEST_PG_DSN names a server to use.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/ethanhinson/fuse/internal/artifact"
	"github.com/ethanhinson/fuse/internal/artifact/artifacttest"
)

// pgHarness is the package-shared Postgres, started once (lazily) and reused
// across tests. When the container runtime is unavailable, skipReason is set
// and every test t.Skips.
type pgHarness struct {
	dsn        string
	skipReason string
	container  *tcpostgres.PostgresContainer
}

var (
	harnessOnce sync.Once
	harness     pgHarness
)

func getHarness() pgHarness {
	harnessOnce.Do(func() {
		if dsn := os.Getenv("FUSE_TEST_PG_DSN"); dsn != "" {
			harness.dsn = dsn
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
			harness.skipReason = fmt.Sprintf("no container runtime: %v", err)
			return
		}
		dsn, err := container.ConnectionString(ctx, "sslmode=disable")
		if err != nil {
			_ = testcontainers.TerminateContainer(container)
			harness.skipReason = fmt.Sprintf("connection string: %v", err)
			return
		}
		harness.container = container
		harness.dsn = dsn
	})
	return harness
}

func dsnOrSkip(t *testing.T) string {
	t.Helper()
	h := getHarness()
	if h.skipReason != "" {
		t.Skip(h.skipReason)
	}
	return h.dsn
}

// openClean opens a Store against the shared database and truncates the
// artifacts table, so each caller starts from an empty store.
func openClean(t *testing.T) *Store {
	t.Helper()
	dsn := dsnOrSkip(t)
	ctx := context.Background()
	s, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `TRUNCATE artifacts`); err != nil {
		s.Close()
		t.Fatalf("truncate: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

func TestPGStoreConformance(t *testing.T) {
	_ = dsnOrSkip(t)
	artifacttest.RunStoreConformance(t, func(t *testing.T) artifact.Store { return openClean(t) })
}

func TestPGStoreBodyCompressed(t *testing.T) {
	s := openClean(t)
	ctx := context.Background()
	ref := artifact.Ref{Tenant: "t1", Loop: "l1", Kind: artifact.KindSpill, ID: "gz"}
	if _, err := s.Put(ctx, ref, strings.NewReader(strings.Repeat("compress me ", 1000)), artifact.Meta{}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	var body []byte
	if err := s.pool.QueryRow(ctx,
		`SELECT body FROM artifacts WHERE tenant_id=$1 AND loop_id=$2 AND kind=$3 AND id=$4`,
		"t1", "l1", "spill", "gz").Scan(&body); err != nil {
		t.Fatalf("select body: %v", err)
	}
	if !bytes.HasPrefix(body, []byte{0x1f, 0x8b}) {
		t.Fatalf("stored body is not gzip: % x", body[:min(len(body), 8)])
	}
}

// TestPGStoreCrossInstance: two Store handles on one database. A writes and B
// reads — the shared-store, different-runtime property.
func TestPGStoreCrossInstance(t *testing.T) {
	a := openClean(t)
	b, err := Open(context.Background(), dsnOrSkip(t))
	if err != nil {
		t.Fatalf("Open B: %v", err)
	}
	t.Cleanup(b.Close)
	ctx := context.Background()
	m, err := a.Put(ctx, artifact.Ref{Tenant: "t1", Loop: "l1", Kind: artifact.KindSegment}, strings.NewReader("shared"), artifact.Meta{Turn: 3})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	rc, meta, err := b.Open(ctx, m.Ref)
	if err != nil {
		t.Fatalf("Open via B: %v", err)
	}
	defer rc.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(rc); err != nil || buf.String() != "shared" || meta.Turn != 3 {
		t.Fatalf("B read %q turn=%d err=%v", buf.String(), meta.Turn, err)
	}
}
