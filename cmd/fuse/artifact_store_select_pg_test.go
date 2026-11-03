//go:build pgstore

package main

import (
	"os"
	"testing"

	artfs "github.com/ethanhinson/fuse/internal/artifact/fsstore"
	artpg "github.com/ethanhinson/fuse/internal/artifact/pgstore"
	"github.com/ethanhinson/fuse/internal/config"
)

// TestSelectArtifactStorePGWithDSN: a tagged binary with a Postgres DSN selects
// the Postgres artifact store. It needs a reachable server, so it skips unless
// FUSE_PG_DSN names one.
func TestSelectArtifactStorePGWithDSN(t *testing.T) {
	if os.Getenv("FUSE_PG_DSN") == "" {
		t.Skip("FUSE_PG_DSN not set")
	}
	s, err := selectArtifactStore(config.Config{})
	if err != nil {
		t.Fatalf("selectArtifactStore: %v", err)
	}
	pg, ok := s.(*artpg.Store)
	if !ok {
		t.Fatalf("selectArtifactStore = %T, want *pgstore.Store", s)
	}
	pg.Close()
}

// TestSelectArtifactStorePGNoDSNFallsBackToFS: with no DSN a tagged binary still
// works locally, on the filesystem artifact store.
func TestSelectArtifactStorePGNoDSNFallsBackToFS(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("FUSE_PG_DSN", "")
	t.Setenv("DATABASE_URL", "")
	s, err := selectArtifactStore(config.Config{})
	if err != nil {
		t.Fatalf("selectArtifactStore: %v", err)
	}
	if _, ok := s.(*artfs.Store); !ok {
		t.Fatalf("selectArtifactStore = %T, want *fsstore.Store", s)
	}
}
