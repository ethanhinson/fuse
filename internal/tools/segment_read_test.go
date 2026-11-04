package tools_test

// External test package: the segments are seeded through artsink, which imports
// internal/agent, which imports internal/tools.

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/artifact"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/segment"
	"github.com/ethanhinson/fuse/internal/segment/artsink"
	"github.com/ethanhinson/fuse/internal/tools"
)

// testBinding is a fresh MemStore binding for one loop.
func testBinding() artifact.Binding {
	return artifact.Binding{Store: artifact.NewMemStore(), Tenant: "acme", Loop: "loop-seg"}
}

// archiveTestSegments archives each segment through the production sink.
func archiveTestSegments(t *testing.T, b artifact.Binding, segs []segment.Segment) {
	t.Helper()
	sink := artsink.New(b)
	if sink == nil {
		t.Fatal("artsink.New returned nil for a valid binding")
	}
	for _, s := range segs {
		if _, err := sink.Archive(agent.SegmentRegion{
			TurnStart: s.TurnStart, TurnEnd: s.TurnEnd, Messages: s.Messages,
			Summary: s.Summary, ToolNames: s.Tools,
		}); err != nil {
			t.Fatalf("archive: %v", err)
		}
	}
}

func execSegRead(t *testing.T, b artifact.Binding, args string) tools.Result {
	t.Helper()
	return tools.NewSegmentRead().Execute(artifact.WithBinding(context.Background(), b), args)
}

func TestSegmentReadSingleTurn(t *testing.T) {
	b := testBinding()
	archiveTestSegments(t, b, []segment.Segment{
		{TurnStart: 10, TurnEnd: 14, Tools: []string{"read_file"},
			Summary: "s1", Messages: msgs("read_file", "alpha content")},
		{TurnStart: 20, TurnEnd: 24, Tools: []string{"grep"},
			Summary: "s2", Messages: msgs("grep", "beta content")},
	})

	// Single turn "12" overlaps only the 10-14 segment.
	res := execSegRead(t, b, `{"turn_range":"12"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if !strings.Contains(res.Output, "alpha content") {
		t.Errorf("expected alpha content, got:\n%s", res.Output)
	}
	if strings.Contains(res.Output, "beta content") {
		t.Errorf("turn 12 must not include the 20-24 segment")
	}
}

// TestSegmentReadNoBindingNotActive: a session with no artifact binding gets the
// clean "not active" result, never an error.
func TestSegmentReadNoBindingNotActive(t *testing.T) {
	for name, ctx := range map[string]context.Context{
		"no binding":      context.Background(),
		"invalid binding": artifact.WithBinding(context.Background(), artifact.Binding{}),
	} {
		res := tools.NewSegmentRead().Execute(ctx, `{"turn_range":"1-5"}`)
		if res.IsError {
			t.Errorf("%s: must not be an error: %s", name, res.Output)
		}
		if res.Output != "no segments — segment archiving is not active for this session" {
			t.Errorf("%s: output = %q", name, res.Output)
		}
	}
}

// TestSegmentReadOtherTenantSeesNothing: tenant A archives a sentinel; a binding
// for tenant B over the same store and loop id must not recover it.
func TestSegmentReadOtherTenantSeesNothing(t *testing.T) {
	b := testBinding()
	archiveTestSegments(t, b, []segment.Segment{
		{TurnStart: 1, TurnEnd: 3, Tools: []string{"read_file"}, Summary: "s",
			Messages: msgs("read_file", "TENANT-A-SENTINEL")},
	})
	if res := execSegRead(t, b, `{"turn_range":"1-3"}`); !strings.Contains(res.Output, "TENANT-A-SENTINEL") {
		t.Fatalf("owning loop must recover its sentinel; got %q", res.Output)
	}
	other := artifact.Binding{Store: b.Store, Tenant: "other", Loop: b.Loop}
	res := execSegRead(t, other, `{"turn_range":"1-3"}`)
	if res.IsError || strings.Contains(res.Output, "TENANT-A-SENTINEL") {
		t.Fatalf("another tenant must not read this loop's segments; got %q", res.Output)
	}
}

// TestSegmentReadMixedCompressionParity is the recovery-tool parity tier: one
// segment artifact holds plaintext bytes and one holds gzipped bytes; a
// segment_read over a turn_range spanning both must recover identical text
// regardless of the stored encoding.
func TestSegmentReadMixedCompressionParity(t *testing.T) {
	b := testBinding()
	ctx := context.Background()

	plainSeg := segment.Segment{TurnStart: 10, TurnEnd: 14, Tools: []string{"read_file"},
		Summary: "s1", Messages: msgs("read_file", "PLAINTEXT-alpha-4217")}
	gzSeg := segment.Segment{TurnStart: 16, TurnEnd: 22, Tools: []string{"grep"},
		Summary: "s2", Messages: msgs("grep", "GZIPPED-beta-4217")}

	plainMeta, err := b.Put(ctx, artifact.KindSegment, strings.NewReader(segment.RenderSegment(plainSeg)), artifact.Meta{})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(segment.RenderSegment(gzSeg))); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	gzMeta, err := b.Put(ctx, artifact.KindSegment, &buf, artifact.Meta{})
	if err != nil {
		t.Fatal(err)
	}

	idx := segment.Index{SessionID: "mixed", Segments: []segment.IndexEntry{
		{TurnStart: 10, TurnEnd: 14, Tools: []string{"read_file"}, Path: plainMeta.Ref.ID},
		{TurnStart: 16, TurnEnd: 22, Tools: []string{"grep"}, Path: gzMeta.Ref.ID},
	}}
	raw, _ := json.Marshal(idx)
	if _, err := b.PutID(ctx, artifact.KindSegmentIndex, segment.IndexArtifactID, bytes.NewReader(raw), artifact.Meta{}); err != nil {
		t.Fatal(err)
	}

	// Range "12-18" overlaps both the plaintext and the gzipped segment.
	res := execSegRead(t, b, `{"turn_range":"12-18"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if !strings.Contains(res.Output, "PLAINTEXT-alpha-4217") {
		t.Errorf("plaintext-backed segment not recovered:\n%s", res.Output)
	}
	if !strings.Contains(res.Output, "GZIPPED-beta-4217") {
		t.Errorf("gz-backed segment not recovered — transparency broken:\n%s", res.Output)
	}
}

func TestSegmentReadRangeOverlap(t *testing.T) {
	b := testBinding()
	// Archived newest first; recovered oldest turn first.
	archiveTestSegments(t, b, []segment.Segment{
		{TurnStart: 16, TurnEnd: 22, Tools: []string{"grep"},
			Summary: "s2", Messages: msgs("grep", "beta content")},
		{TurnStart: 10, TurnEnd: 14, Tools: []string{"read_file"},
			Summary: "s1", Messages: msgs("read_file", "alpha content")},
	})

	// Range "12-18" overlaps both segments (10-14 and 16-22).
	res := execSegRead(t, b, `{"turn_range":"12-18"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	a, z := strings.Index(res.Output, "alpha content"), strings.Index(res.Output, "beta content")
	if a < 0 || z < 0 {
		t.Fatalf("range 12-18 must include both overlapping segments; got:\n%s", res.Output)
	}
	if a > z {
		t.Errorf("segments must be ordered by turn; got:\n%s", res.Output)
	}
}

func TestSegmentReadToolFilter(t *testing.T) {
	b := testBinding()
	archiveTestSegments(t, b, []segment.Segment{
		{TurnStart: 10, TurnEnd: 20, Tools: []string{"read_file", "grep"},
			Summary: "s1", Messages: []model.Message{
				{Role: "tool", Name: "read_file", Content: "file body here"},
				{Role: "tool", Name: "grep", Content: "grep body here"},
			}},
	})

	res := execSegRead(t, b, `{"turn_range":"10-20","tool_filter":"read_file"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if !strings.Contains(res.Output, "file body here") {
		t.Errorf("tool_filter=read_file must keep read_file result; got:\n%s", res.Output)
	}
	if strings.Contains(res.Output, "grep body here") {
		t.Errorf("tool_filter=read_file must drop grep result; got:\n%s", res.Output)
	}
}

func TestSegmentReadEmptyRangeCleanResult(t *testing.T) {
	b := testBinding()
	archiveTestSegments(t, b, []segment.Segment{
		{TurnStart: 10, TurnEnd: 14, Summary: "s1", Messages: msgs("read_file", "x")},
	})
	// Range 100-200 matches nothing: a clean "no segments" result, not an error.
	res := execSegRead(t, b, `{"turn_range":"100-200"}`)
	if res.IsError {
		t.Errorf("empty range must NOT be an error: %s", res.Output)
	}
	if !strings.Contains(strings.ToLower(res.Output), "no segments") {
		t.Errorf("empty range should say 'no segments'; got: %q", res.Output)
	}
}

func TestSegmentReadOversizedBounded(t *testing.T) {
	b := testBinding()
	// Many big segments so the selection exceeds the cap.
	var segs []segment.Segment
	big := strings.Repeat("Q", 20000)
	for i := 0; i < 30; i++ {
		segs = append(segs, segment.Segment{
			TurnStart: i, TurnEnd: i, Tools: []string{"read_file"},
			Summary: "s", Messages: msgs("read_file", big),
		})
	}
	archiveTestSegments(t, b, segs)

	res := execSegRead(t, b, `{"turn_range":"0-29"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if !strings.Contains(res.Output, "more segments") && !strings.Contains(res.Output, "narrow turn_range") {
		t.Errorf("oversized selection must carry a bounded trailer; got tail:\n%s", tail(res.Output, 300))
	}
	if len(res.Output) > 32<<10+512 {
		t.Errorf("output %d bytes exceeds the 32 KiB cap", len(res.Output))
	}
}

// TestSegmentReadThroughRegistryBinding: the registry stamps its binding onto
// the tool's context, so a bound registry's segment_read reads that loop.
func TestSegmentReadThroughRegistryBinding(t *testing.T) {
	b := testBinding()
	archiveTestSegments(t, b, []segment.Segment{
		{TurnStart: 4, TurnEnd: 6, Summary: "s", Messages: msgs("read_file", "VIA-REGISTRY")},
	})
	reg := tools.NewRegistry()
	reg.Register(tools.NewSegmentRead())
	reg.WithArtifacts(b.Store, b.Tenant, b.Loop)
	res := reg.Execute(context.Background(), "segment_read", `{"turn_range":"5"}`)
	if res.IsError || !strings.Contains(res.Output, "VIA-REGISTRY") {
		t.Fatalf("bound registry did not recover the segment: %q", res.Output)
	}
}

func msgs(tool, content string) []model.Message {
	return []model.Message{{Role: "tool", Name: tool, Content: content}}
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
