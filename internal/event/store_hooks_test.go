package event_test

// store_hooks_test.go keeps observability vendors out of the core packages.

import (
	"bytes"
	"os/exec"
	"testing"
)

// TestNoOTELDependency asserts internal/event imports no OTEL in 0047 (D5 is
// hooks-only — no exporter, no emission). The pgstore backend (which transitively
// pulls OTEL via testcontainers in its TESTS) is a separate, build-tagged package
// and is NOT part of internal/event's dependency closure here.
func TestCorePackagesHaveNoObservabilityVendorDependency(t *testing.T) {
	for _, pkg := range []string{
		"github.com/ethanhinson/fuse/internal/event",
		"github.com/ethanhinson/fuse/internal/observe",
		"github.com/ethanhinson/fuse/internal/runtime",
		"github.com/ethanhinson/fuse/internal/agent",
	} {
		out, err := exec.Command("go", "list", "-deps", pkg).Output()
		if err != nil {
			t.Fatalf("go list -deps %s: %v", pkg, err)
		}
		for _, forbidden := range [][]byte{[]byte("go.opentelemetry.io"), []byte("github.com/prometheus")} {
			if bytes.Contains(out, forbidden) {
				t.Fatalf("%s must not import observability vendor dependency %q", pkg, forbidden)
			}
		}
	}
}
