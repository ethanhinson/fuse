package harness

import (
	"slices"
	"testing"
)

// TestSpecFeatures pins the advertised feature names and that each call hands
// back a fresh slice a caller may mutate.
func TestSpecFeatures(t *testing.T) {
	if SpecFeatureNoTools != "no_tools" {
		t.Fatalf("SpecFeatureNoTools = %q, want no_tools", SpecFeatureNoTools)
	}
	got := SpecFeatures()
	if !slices.Equal(got, []string{SpecFeatureNoTools}) {
		t.Fatalf("SpecFeatures() = %v, want [%s]", got, SpecFeatureNoTools)
	}
	got[0] = "mutated"
	if again := SpecFeatures(); !slices.Equal(again, []string{SpecFeatureNoTools}) {
		t.Fatalf("SpecFeatures() after mutating an earlier result = %v, want a fresh slice", again)
	}
}
