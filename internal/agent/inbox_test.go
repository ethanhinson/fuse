package agent

import "testing"

// A value written at InboxKey(target, seq) is discoverable via
// Keys(InboxPattern(target)) and readable via Get — the directed-message inbox
// convention over the plain store (change 0023, Task 6).
func TestInboxConvention(t *testing.T) {
	bb := NewBlackboard(nil)
	target := "research/facet-2"

	bb.Put(InboxKey(target, "1"), "hello", "sender-id", "sender/label")
	bb.Put(InboxKey(target, "2"), "world", "sender-id", "sender/label")
	// A message for a different target must NOT match target's pattern.
	bb.Put(InboxKey("other", "1"), "nope", "sender-id", "sender/label")

	keys := bb.Keys(InboxPattern(target))
	if len(keys) != 2 {
		t.Fatalf("Keys(%q) = %v, want 2 messages", InboxPattern(target), keys)
	}
	want := []string{InboxKey(target, "1"), InboxKey(target, "2")}
	for i, k := range want {
		if keys[i] != k {
			t.Fatalf("keys[%d] = %q, want %q", i, keys[i], k)
		}
	}

	entry, ok := bb.Get(InboxKey(target, "1"))
	if !ok || entry.Value != "hello" {
		t.Fatalf("Get(%q) = (%v, %v), want (hello, true)", InboxKey(target, "1"), entry.Value, ok)
	}
}
