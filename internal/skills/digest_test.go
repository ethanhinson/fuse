package skills

// digest_test.go covers DigestOf's inclusion of the description (fix 4): a
// description is model-visible (it is rendered into the prompt block), so a
// description-only edit must change the digest.

import "testing"

// TestDigestOfIncludesDescription proves two sets differing ONLY in a skill's
// description produce different digests.
func TestDigestOfIncludesDescription(t *testing.T) {
	a := NewSet([]Skill{{Name: "triage", Description: "Sort issues: label and size", Body: "same body"}})
	b := NewSet([]Skill{{Name: "triage", Description: "Sort issues: severity only", Body: "same body"}})
	if gotA, gotB := DigestOf(a), DigestOf(b); gotA == gotB {
		t.Fatalf("DigestOf ignores description: both %q", gotA)
	}
}

// TestDigestOfStableAcrossIdenticalSets pins the other half: identically-built
// sets still agree, so adding description to the mix did not break determinism.
func TestDigestOfStableAcrossIdenticalSets(t *testing.T) {
	build := func() *Set {
		return NewSet([]Skill{
			{Name: "a", Description: "does a", Body: "A"},
			{Name: "b", Description: "does b", Body: "B"},
		})
	}
	if DigestOf(build()) != DigestOf(build()) {
		t.Fatal("identical sets produced different digests")
	}
}
