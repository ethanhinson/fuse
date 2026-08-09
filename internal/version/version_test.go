package version

import "testing"

// TestVersionIsSet asserts only that a version is present. It deliberately does
// NOT pin the exact string: Version is ldflags-injectable, so a release build
// stamps a different value, and a test that pinned "0.1.0-dev" would fail those
// builds (and previously made the const un-injectable).
func TestVersionIsSet(t *testing.T) {
	if Version == "" {
		t.Fatal("Version must not be empty")
	}
}
