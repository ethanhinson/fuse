package agent

import (
	"testing"

	"github.com/ethanhinson/fuse/internal/model"
)

// segment_read is hidden until a compaction has archived a segment it can read.
func TestSegmentReadIsOfferedOnlyAfterAnArchivedCompaction(t *testing.T) {
	schemas := []model.ToolSchema{{Name: "bash"}, {Name: "segment_read"}, {Name: "read_file"}}
	plain := []model.Message{{Role: "user", Content: "task"}, {Role: "assistant", Content: "working"}}
	if hasRecoverableSegment(plain) {
		t.Fatal("no compaction yet")
	}
	if got := withoutTool(schemas, "segment_read"); len(got) != 2 || got[0].Name != "bash" || got[1].Name != "read_file" {
		t.Errorf("withoutTool = %v", got)
	}
	if len(schemas) != 3 {
		t.Error("withoutTool mutated its input")
	}
	noPointer := append(plain, buildSummaryMessage("summary", ""))
	if hasRecoverableSegment(noPointer) {
		t.Error("a summary with nothing archived has nothing to recover")
	}
	archived := append(plain, buildSummaryMessage("summary", "art://x/segment/1"))
	if !hasRecoverableSegment(archived) {
		t.Error("an archived summary should offer segment_read")
	}
}
