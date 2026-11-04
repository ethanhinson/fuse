package artifact_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/ethanhinson/fuse/internal/artifact"
	"github.com/ethanhinson/fuse/internal/artifact/artifacttest"
)

func TestMemStoreConformance(t *testing.T) {
	artifacttest.RunStoreConformance(t, func(t *testing.T) artifact.Store { return artifact.NewMemStore() })
}

func TestMemStorePerLoopCap(t *testing.T) {
	ctx := context.Background()
	s := artifact.NewMemStore(artifact.WithPerLoopCap(100))
	body := func(n int) *bytes.Reader { return bytes.NewReader(bytes.Repeat([]byte("x"), n)) }
	first := artifact.Ref{Tenant: "t", Loop: "l1", Kind: artifact.KindSpill, ID: "first"}
	if _, err := s.Put(ctx, first, body(60), artifact.Meta{}); err != nil {
		t.Fatalf("first Put: %v", err)
	}
	second := artifact.Ref{Tenant: "t", Loop: "l1", Kind: artifact.KindSpill, ID: "second"}
	if _, err := s.Put(ctx, second, body(60), artifact.Meta{}); !errors.Is(err, artifact.ErrQuotaExceeded) {
		t.Fatalf("second Put err = %v, want ErrQuotaExceeded", err)
	}
	if _, _, err := s.Open(ctx, second); !errors.Is(err, artifact.ErrNotFound) {
		t.Fatalf("refused Put left an artifact behind: %v", err)
	}
	other := artifact.Ref{Tenant: "t", Loop: "l2", Kind: artifact.KindSpill, ID: "second"}
	if _, err := s.Put(ctx, other, body(60), artifact.Meta{}); err != nil {
		t.Fatalf("Put on another loop: %v", err)
	}
	if _, err := s.Put(ctx, first, body(30), artifact.Meta{}); err != nil {
		t.Fatalf("overwrite with smaller body: %v", err)
	}
	// 30 used of 100: a 70-byte Put now fits, proving the old 60 was released.
	if _, err := s.Put(ctx, second, body(70), artifact.Meta{}); err != nil {
		t.Fatalf("Put after release: %v", err)
	}
	if artifact.DefaultMemLoopCap != 64<<20 {
		t.Fatalf("DefaultMemLoopCap = %d", artifact.DefaultMemLoopCap)
	}
}
