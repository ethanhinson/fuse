package agent

import "testing"

// ForNode binds provenance (Write) and round-trips through Read via the
// tools.BlackboardStore seam.
func TestForNodeStampsProvenance(t *testing.T) {
	bb := NewBlackboard(nil)
	node := &AgentNode{ID: "node-1", Label: "research/facet-2"}
	store := bb.ForNode(node)

	store.Write("k", map[string]any{"a": float64(1)})

	// Provenance is stamped from the node, not supplied by the caller.
	entry, ok := bb.Get("k")
	if !ok {
		t.Fatalf("Get after Write: key not present")
	}
	if entry.WriterID != "node-1" || entry.WriterLabel != "research/facet-2" {
		t.Fatalf("provenance = (%q, %q), want (node-1, research/facet-2)", entry.WriterID, entry.WriterLabel)
	}

	// Read round-trips value + writer label.
	value, label, found := store.Read("k")
	if !found {
		t.Fatalf("Read after Write: not found")
	}
	if label != "research/facet-2" {
		t.Fatalf("Read label = %q, want research/facet-2", label)
	}
	m, ok := value.(map[string]any)
	if !ok || m["a"] != float64(1) {
		t.Fatalf("Read value = %#v, want map[a:1]", value)
	}

	// Missing key: not found, empty label.
	if _, _, found := store.Read("missing"); found {
		t.Fatalf("Read missing: got found=true")
	}
}
