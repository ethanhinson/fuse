// Package artifacttest holds the conformance suite every artifact.Store
// implementation must pass.
package artifacttest

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/artifact"
	"github.com/ethanhinson/fuse/internal/event"
)

// RunStoreConformance runs the shared Store contract against fresh stores from
// newStore, one per subtest.
func RunStoreConformance(t *testing.T, newStore func(t *testing.T) artifact.Store) {
	t.Helper()
	ctx := context.Background()

	ref := func(tenant, loop string, kind artifact.Kind, id string) artifact.Ref {
		return artifact.Ref{Tenant: event.TenantID(tenant), Loop: event.LoopID(loop), Kind: kind, ID: id}
	}
	readAll := func(t *testing.T, s artifact.Store, r artifact.Ref) ([]byte, artifact.Meta) {
		t.Helper()
		rc, meta, err := s.Open(ctx, r)
		if err != nil {
			t.Fatalf("Open(%s): %v", r, err)
		}
		defer rc.Close()
		body, err := io.ReadAll(rc)
		if err != nil {
			t.Fatalf("read %s: %v", r, err)
		}
		return body, meta
	}

	t.Run("RoundTrip", func(t *testing.T) {
		s := newStore(t)
		r := ref("t1", "l1", artifact.KindSpill, "rt1")
		in := artifact.Meta{Tool: "bash", Turn: 7, Labels: map[string]string{"k": "v"}}
		put, err := s.Put(ctx, r, strings.NewReader("hello artifact"), in)
		if err != nil {
			t.Fatalf("Put: %v", err)
		}
		if put.Ref != r || put.Size != int64(len("hello artifact")) || put.CreatedAt.IsZero() {
			t.Fatalf("Put meta = %+v", put)
		}
		body, meta := readAll(t, s, r)
		if string(body) != "hello artifact" {
			t.Fatalf("body = %q", body)
		}
		if meta.Ref != r || meta.Size != int64(len(body)) || meta.Tool != "bash" || meta.Turn != 7 ||
			!maps.Equal(meta.Labels, in.Labels) {
			t.Fatalf("Open meta = %+v", meta)
		}
	})

	t.Run("AllocatedIDsUniqueUnderConcurrency", func(t *testing.T) {
		s := newStore(t)
		const n = 50
		ids := make([]string, n)
		errs := make([]error, n)
		var wg sync.WaitGroup
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				m, err := s.Put(ctx, ref("t1", "l1", artifact.KindSpill, ""), strings.NewReader(fmt.Sprintf("body-%d", i)), artifact.Meta{})
				ids[i], errs[i] = m.Ref.ID, err
			}()
		}
		wg.Wait()
		seen := map[string]bool{}
		for i, id := range ids {
			if errs[i] != nil {
				t.Fatalf("Put %d: %v", i, errs[i])
			}
			if id == "" || seen[id] {
				t.Fatalf("id %q empty or duplicated", id)
			}
			seen[id] = true
			readAll(t, s, ref("t1", "l1", artifact.KindSpill, id))
		}
		list, err := s.List(ctx, "t1", "l1", artifact.KindSpill)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(list) != n {
			t.Fatalf("List returned %d, want %d", len(list), n)
		}
	})

	t.Run("ListOrderAndScoping", func(t *testing.T) {
		s := newStore(t)
		put := func(r artifact.Ref) {
			t.Helper()
			if _, err := s.Put(ctx, r, strings.NewReader(r.String()), artifact.Meta{}); err != nil {
				t.Fatalf("Put(%s): %v", r, err)
			}
		}
		want := []string{"a1", "a2", "a3"}
		for _, id := range want {
			put(ref("t1", "l1", artifact.KindSpill, id))
			time.Sleep(5 * time.Millisecond)
		}
		put(ref("t1", "l1", artifact.KindSegment, "other-kind"))
		put(ref("t1", "l2", artifact.KindSpill, "other-loop"))
		put(ref("t2", "l1", artifact.KindSpill, "other-tenant"))
		list, err := s.List(ctx, "t1", "l1", artifact.KindSpill)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		var got []string
		for _, m := range list {
			got = append(got, m.Ref.ID)
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("List ids = %v, want %v", got, want)
		}
	})

	t.Run("WrongTenantOpenIsNotFound", func(t *testing.T) {
		s := newStore(t)
		if _, err := s.Put(ctx, ref("ta", "l1", artifact.KindSpill, "secret"), strings.NewReader("tenant-a sentinel"), artifact.Meta{}); err != nil {
			t.Fatalf("Put: %v", err)
		}
		// Positive control: the owner can read it.
		readAll(t, s, ref("ta", "l1", artifact.KindSpill, "secret"))
		if _, _, err := s.Open(ctx, ref("tb", "l1", artifact.KindSpill, "secret")); !errors.Is(err, artifact.ErrNotFound) {
			t.Fatalf("cross-tenant Open err = %v, want ErrNotFound", err)
		}
	})

	t.Run("OverwriteReplaces", func(t *testing.T) {
		s := newStore(t)
		r := ref("t1", "l1", artifact.KindSegmentIndex, "index")
		for _, b := range []string{"first", "second"} {
			if _, err := s.Put(ctx, r, strings.NewReader(b), artifact.Meta{}); err != nil {
				t.Fatalf("Put %s: %v", b, err)
			}
		}
		if body, _ := readAll(t, s, r); string(body) != "second" {
			t.Fatalf("body = %q, want second", body)
		}
		list, err := s.List(ctx, "t1", "l1", artifact.KindSegmentIndex)
		if err != nil || len(list) != 1 {
			t.Fatalf("List = %v, %v; want one entry", list, err)
		}
	})

	t.Run("CompressionTransparent", func(t *testing.T) {
		s := newStore(t)
		plain := bytes.Repeat([]byte("compressible "), (1<<20)/13+1)[:1<<20]
		var gz bytes.Buffer
		zw := gzip.NewWriter(&gz)
		_, _ = zw.Write([]byte("caller-owned gzip payload"))
		_ = zw.Close()
		for id, in := range map[string][]byte{"plain": plain, "gzipped": gz.Bytes()} {
			r := ref("t1", "l1", artifact.KindSegment, id)
			if _, err := s.Put(ctx, r, bytes.NewReader(in), artifact.Meta{}); err != nil {
				t.Fatalf("Put %s: %v", id, err)
			}
			body, meta := readAll(t, s, r)
			if !bytes.Equal(body, in) {
				t.Fatalf("%s body not byte-identical (got %d bytes, want %d)", id, len(body), len(in))
			}
			if meta.Size != int64(len(in)) {
				t.Fatalf("%s Size = %d, want %d", id, meta.Size, len(in))
			}
		}
	})

	t.Run("SizeCap", func(t *testing.T) {
		s := newStore(t)
		r := ref("t1", "l1", artifact.KindSpill, "huge")
		big := io.LimitReader(zeroReader{}, artifact.MaxArtifactBytes+1)
		if _, err := s.Put(ctx, r, big, artifact.Meta{}); !errors.Is(err, artifact.ErrTooLarge) {
			t.Fatalf("Put err = %v, want ErrTooLarge", err)
		}
		if _, _, err := s.Open(ctx, r); !errors.Is(err, artifact.ErrNotFound) {
			t.Fatalf("Open after refused Put err = %v, want ErrNotFound", err)
		}
	})

	t.Run("OpenMissing", func(t *testing.T) {
		s := newStore(t)
		if _, _, err := s.Open(ctx, ref("t1", "l1", artifact.KindSpill, "nope")); !errors.Is(err, artifact.ErrNotFound) {
			t.Fatalf("Open err = %v, want ErrNotFound", err)
		}
	})

	t.Run("InvalidRefRefused", func(t *testing.T) {
		s := newStore(t)
		for _, r := range []artifact.Ref{
			ref("t1", "..", artifact.KindSpill, "x"),
			ref("t1", "l1", artifact.KindSpill, ".."),
			ref("..", "l1", artifact.KindSpill, "x"),
		} {
			if _, err := s.Put(ctx, r, strings.NewReader("x"), artifact.Meta{}); !errors.Is(err, artifact.ErrInvalidRef) {
				t.Fatalf("Put(%s) err = %v, want ErrInvalidRef", r, err)
			}
		}
	})
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}
