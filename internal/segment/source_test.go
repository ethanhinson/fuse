package segment

import (
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/artifact"
	"github.com/ethanhinson/fuse/internal/model"
)

// TestParseSegmentMatchesLoadSegment: ParseSegment over the raw bytes, gzip or
// plain, yields exactly what LoadSegment yields for the same file.
func TestParseSegmentMatchesLoadSegment(t *testing.T) {
	seg := Segment{
		TurnStart: 2, TurnEnd: 4, TS: time.Now().UTC(), Summary: "the summary",
		Messages: []model.Message{
			{Role: "tool", Name: "grep", Content: "needle 9137"},
			{Role: "assistant", Content: "ok"},
		},
	}
	plain := []byte(RenderSegment(seg))
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	for name, b := range map[string][]byte{"plain": plain, "gzip": gz.Bytes()} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "seg.md")
			if err := os.WriteFile(path, b, 0o600); err != nil {
				t.Fatal(err)
			}
			want, err := LoadSegment(path)
			if err != nil {
				t.Fatalf("LoadSegment: %v", err)
			}
			got, err := ParseSegment(b)
			if err != nil {
				t.Fatalf("ParseSegment: %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("ParseSegment = %+v, LoadSegment = %+v", got, want)
			}
			if got.Summary != "the summary" || len(got.Messages) != 2 || got.Messages[0].Content != "needle 9137" {
				t.Fatalf("unexpected parse: %+v", got)
			}
		})
	}
}

func TestArtifactSourceInvalidBindingIsEmpty(t *testing.T) {
	src := NewArtifactSource(artifact.Binding{})
	idx, err := src.ReadIndex(context.Background())
	if err != nil || len(idx.Segments) != 0 {
		t.Fatalf("ReadIndex = %+v, %v; want empty, nil", idx, err)
	}
	if _, err := src.Load(context.Background(), IndexEntry{Path: "x"}); err == nil {
		t.Fatal("Load on invalid binding: want error")
	}
}

func TestArtifactSourceMissingIndexIsEmpty(t *testing.T) {
	b := artifact.Binding{Store: artifact.NewMemStore(), Tenant: "t1", Loop: "l1"}
	idx, err := NewArtifactSource(b).ReadIndex(context.Background())
	if err != nil || len(idx.Segments) != 0 {
		t.Fatalf("ReadIndex = %+v, %v; want empty, nil", idx, err)
	}
}

// TestArtifactSourceTenantIsolation: tenant A's segment index is invisible to a
// source bound to tenant B on the same store and loop id.
func TestArtifactSourceTenantIsolation(t *testing.T) {
	ctx := context.Background()
	store := artifact.NewMemStore()
	a := artifact.Binding{Store: store, Tenant: "tenant-a", Loop: "loop-1"}
	b := artifact.Binding{Store: store, Tenant: "tenant-b", Loop: "loop-1"}
	if _, err := a.PutID(ctx, artifact.KindSegmentIndex, IndexArtifactID,
		strings.NewReader(`{"session_id":"loop-1","segments":[{"path":"sentinel"}]}`), artifact.Meta{}); err != nil {
		t.Fatal(err)
	}
	if idx, err := NewArtifactSource(a).ReadIndex(ctx); err != nil || len(idx.Segments) != 1 {
		t.Fatalf("tenant A ReadIndex = %+v, %v; want its sentinel", idx, err)
	}
	if idx, err := NewArtifactSource(b).ReadIndex(ctx); err != nil || len(idx.Segments) != 0 {
		t.Fatalf("tenant B ReadIndex = %+v, %v; want empty", idx, err)
	}
}
