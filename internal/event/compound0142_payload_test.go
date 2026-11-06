package event

import (
	"encoding/json"
	"testing"
)

// The wire payload for a compound bash command carries the per-segment
// deterministic verdicts (change 0142) so the permission.decision audit event
// attributes a compound outcome to the specific part that drove it.
func TestPermissionDecisionPayload_SegmentsRoundTrip(t *testing.T) {
	p := PermissionDecisionPayload{
		Tool:    "bash",
		Verdict: "allow",
		Layer:   "heuristic",
		Mode:    "auto",
		Command: `cd "$(pwd)" && git status`,
		Segments: []SegmentVerdictPayload{
			{Name: "cd", Verdict: "allow"},
			{Name: "git", Verdict: "allow"},
		},
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got PermissionDecisionPayload
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Segments) != 2 || got.Segments[0].Name != "cd" || got.Segments[1].Name != "git" {
		t.Fatalf("segments did not round-trip: %+v", got.Segments)
	}
}

// A non-compound decision omits the segments field entirely (omitempty), so
// the pre-0142 event shape is byte-identical for the common case.
func TestPermissionDecisionPayload_NoSegmentsOmitsField(t *testing.T) {
	p := PermissionDecisionPayload{Tool: "bash", Verdict: "allow", Layer: "safelist", Mode: "auto"}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, present := m["segments"]; present {
		t.Fatalf("segments key present without segments: %s", b)
	}
}
