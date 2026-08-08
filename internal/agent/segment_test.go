package agent

import (
	"testing"

	"github.com/ethanhinson/fuse/internal/model"
)

// fakeSink returns a fixed pointer and records the last region it saw.
type fakeSink struct {
	pointer string
	err     error
	last    SegmentRegion
	calls   int
}

func (f *fakeSink) Archive(r SegmentRegion) (string, error) {
	f.calls++
	f.last = r
	return f.pointer, f.err
}

func TestNoopSegmentSinkReturnsEmpty(t *testing.T) {
	var s noopSegmentSink
	region := SegmentRegion{
		TurnStart:    1,
		TurnEnd:      3,
		Messages:     []model.Message{{Role: "tool", Content: "x"}},
		Summary:      "some summary",
		ToolNames:    []string{"read_file"},
		TokensBefore: 100,
		TokensAfter:  10,
	}
	ptr, err := s.Archive(region)
	if err != nil {
		t.Fatalf("noop Archive err = %v, want nil", err)
	}
	if ptr != "" {
		t.Errorf("noop Archive pointer = %q, want empty", ptr)
	}
}

func TestNoopSegmentSinkSatisfiesInterface(t *testing.T) {
	// Compile-time + runtime: the no-op is a valid SegmentSink.
	var _ SegmentSink = noopSegmentSink{}
}

func TestFakeSinkReturnsPointer(t *testing.T) {
	f := &fakeSink{pointer: "/path/to/segment"}
	var sink SegmentSink = f
	ptr, err := sink.Archive(SegmentRegion{TurnStart: 2, TurnEnd: 5})
	if err != nil {
		t.Fatalf("Archive err = %v", err)
	}
	if ptr != "/path/to/segment" {
		t.Errorf("pointer = %q, want /path/to/segment", ptr)
	}
	if f.last.TurnStart != 2 || f.last.TurnEnd != 5 {
		t.Errorf("recorded region turn range = %d..%d, want 2..5", f.last.TurnStart, f.last.TurnEnd)
	}
}
