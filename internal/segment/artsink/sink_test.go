package artsink_test

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"testing"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/artifact"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/segment"
	"github.com/ethanhinson/fuse/internal/segment/artsink"
)

var _ agent.SegmentSink = (*artsink.Sink)(nil)

func memBinding() artifact.Binding {
	return artifact.Binding{Store: artifact.NewMemStore(), Tenant: "tenant-a", Loop: "loop-1"}
}

func region(start, end int, content string) agent.SegmentRegion {
	return agent.SegmentRegion{
		TurnStart: start, TurnEnd: end,
		Messages: []model.Message{
			{Role: "tool", Name: "read_file", Content: content},
			{Role: "assistant", Content: "ack"},
		},
		Summary:      "summary",
		ToolNames:    []string{"read_file"},
		TokensBefore: 1200,
		TokensAfter:  80,
	}
}

func TestArchiveWritesSegmentAndIndex(t *testing.T) {
	ctx := context.Background()
	b := memBinding()
	sink := artsink.New(b)
	r := region(3, 7, "RAWDATA 4217")
	ptr, err := sink.Archive(r)
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	ref, err := artifact.ParseRef(ptr)
	if err != nil {
		t.Fatalf("pointer %q does not parse: %v", ptr, err)
	}
	if ref.Kind != artifact.KindSegment || ref.Tenant != b.Tenant || ref.Loop != b.Loop {
		t.Fatalf("pointer ref = %+v, want segment under %s/%s", ref, b.Tenant, b.Loop)
	}

	src := segment.NewArtifactSource(b)
	idx, err := src.ReadIndex(ctx)
	if err != nil {
		t.Fatalf("ReadIndex: %v", err)
	}
	if len(idx.Segments) != 1 {
		t.Fatalf("index entries = %d, want 1", len(idx.Segments))
	}
	e := idx.Segments[0]
	if e.Path != ref.ID {
		t.Fatalf("entry Path = %q, want ref ID %q", e.Path, ref.ID)
	}
	if e.TurnStart != 3 || e.TurnEnd != 7 || !reflect.DeepEqual(e.Tools, []string{"read_file"}) ||
		e.TokensBefore != 1200 || e.TokensAfter != 80 {
		t.Fatalf("entry = %+v", e)
	}
	seg, err := src.Load(ctx, e)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(seg.Messages, r.Messages) || seg.Summary != "summary" {
		t.Fatalf("loaded segment = %+v, want messages %+v", seg, r.Messages)
	}
}

// TestArchiveAppendsAcrossSinks: two sinks over one store (two runtimes serving
// the same loop) extend one index; neither overwrites the other's segment.
func TestArchiveAppendsAcrossSinks(t *testing.T) {
	ctx := context.Background()
	b := memBinding()
	p1, err := artsink.New(b).Archive(region(1, 2, "first"))
	if err != nil {
		t.Fatal(err)
	}
	p2, err := artsink.New(b).Archive(region(1, 2, "second"))
	if err != nil {
		t.Fatal(err)
	}
	if p1 == p2 {
		t.Fatalf("both sinks returned the same pointer %q", p1)
	}
	src := segment.NewArtifactSource(b)
	idx, err := src.ReadIndex(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.Segments) != 2 {
		t.Fatalf("index entries = %d, want 2", len(idx.Segments))
	}
	for i, want := range []string{"first", "second"} {
		seg, err := src.Load(ctx, idx.Segments[i])
		if err != nil {
			t.Fatalf("Load %d: %v", i, err)
		}
		if seg.Messages[0].Content != want {
			t.Fatalf("segment %d content = %q, want %q", i, seg.Messages[0].Content, want)
		}
	}
}

func TestArchiveConcurrent(t *testing.T) {
	b := memBinding()
	sink := artsink.New(b)
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := sink.Archive(region(i, i+1, fmt.Sprint("c", i))); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	idx, err := segment.NewArtifactSource(b).ReadIndex(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.Segments) != 20 {
		t.Fatalf("index entries = %d, want 20", len(idx.Segments))
	}
	seen := map[string]bool{}
	for _, e := range idx.Segments {
		if seen[e.Path] {
			t.Fatalf("duplicate segment id %q", e.Path)
		}
		seen[e.Path] = true
	}
}

func TestNewInvalidBindingIsNil(t *testing.T) {
	if s := artsink.New(artifact.Binding{}); s != nil {
		t.Fatalf("New(invalid) = %#v, want nil interface", s)
	}
}
