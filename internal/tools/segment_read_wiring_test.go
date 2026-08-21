package tools

// Child-registry wiring re-verification for segment_read (change 0030, learning
// patch-every-cloned-child-builder). Every child tool registry in cmd/fuse is
// built by childToolRegistry, which either Clones the parent (empty request →
// full inheritance) or Subsets it by name. This test pins that segment_read is
// in DefaultTools(nil) (so it reaches every parent registry), survives a Clone, and
// is selected by a Subset that names it — the two shapes childToolRegistry
// produces.

import "testing"

func hasTool(reg *Registry, name string) bool {
	for _, t := range reg.Tools() {
		if t.Name() == name {
			return true
		}
	}
	return false
}

func TestSegmentReadInDefaultTools(t *testing.T) {
	found := false
	for _, tl := range DefaultTools(nil) {
		if tl.Name() == "segment_read" {
			found = true
		}
	}
	if !found {
		t.Fatal("segment_read missing from DefaultTools(nil) — children built from it won't have recovery")
	}
}

func TestSegmentReadSurvivesCloneAndSubset(t *testing.T) {
	parent := NewRegistry()
	for _, tl := range DefaultTools(nil) {
		parent.Register(tl)
	}

	// Clone: the empty-request child path (parent.Clone()) must carry it.
	clone := parent.Clone()
	if !hasTool(clone, "segment_read") {
		t.Error("segment_read dropped by Clone() — an empty-tools child loses recovery")
	}

	// Subset naming it: present.
	sub, unknown := parent.Subset([]string{"grep", "segment_read"})
	if len(unknown) != 0 {
		t.Fatalf("unexpected unknown tools: %v", unknown)
	}
	if !hasTool(sub, "segment_read") {
		t.Error("segment_read dropped by a Subset that names it")
	}

	// Subset omitting it: absent (a child can deliberately withhold it — fine).
	subOmit, _ := parent.Subset([]string{"grep"})
	if hasTool(subOmit, "segment_read") {
		t.Error("Subset that omits segment_read must not include it")
	}
}
