package agent

import "testing"

func TestDefaultContextWindowIsExported(t *testing.T) {
	if DefaultContextWindow != 128_000 {
		t.Fatalf("DefaultContextWindow = %d, want 128000", DefaultContextWindow)
	}
}
