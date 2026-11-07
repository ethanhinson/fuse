package sandbox

import (
	"strings"
	"testing"
)

// Every neighbouring spelling must be unrecognised, including the retired Kubernetes
// substrate: no prefix or fuzzy matching, because that is how a typo becomes a substrate
// nobody chose.
func TestLoadConfigHandlerNearMissesStillUnknown(t *testing.T) {
	for _, spelling := range []string{"k8s", "kube", "kubernetes", "containers", "hosts"} {
		t.Run(spelling, func(t *testing.T) {
			root := t.TempDir()
			writeConfigFile(t, root, "handler: "+spelling+"\n")

			cfg, warns := LoadConfig(root)

			assertContained(t, cfg)
			if !hasWarning(warns, WarnUnknownHandler) {
				t.Fatalf("warnings = %v, want a %q warning", warns, WarnUnknownHandler)
			}
		})
	}
}

// The unknown-handler diagnostic must name the valid values.
func TestUnknownHandlerDetailNamesEveryValidHandler(t *testing.T) {
	root := t.TempDir()
	writeConfigFile(t, root, "handler: k8s\n")

	_, warns := LoadConfig(root)

	var detail string
	for _, w := range warns {
		if w.Reason == WarnUnknownHandler {
			detail = w.Detail
		}
	}
	if detail == "" {
		t.Fatalf("warnings = %v, want a %q warning", warns, WarnUnknownHandler)
	}
	for _, want := range []string{HandlerContainer, HandlerHost} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail = %q, want it to name %q", detail, want)
		}
	}
}

// Every WarnReason is distinct: two reasons sharing a string would collapse two causes
// an operator cannot then tell apart.
func TestWarnReasonValuesAreDistinct(t *testing.T) {
	want := []WarnReason{
		WarnNoRoot,
		WarnUnreadable,
		WarnMalformed,
		WarnUnknownHandler,
		WarnContradictory,
		WarnBadIdleTTL,
		WarnBadLimit,
		WarnBadConcurrency,
		WarnUnknownEgressMode,
		WarnBadEgress,
	}
	seen := make(map[WarnReason]bool, len(want))
	for _, r := range want {
		if r == "" {
			t.Error("a WarnReason is the empty string")
		}
		if seen[r] {
			t.Errorf("WarnReason %q is declared twice", r)
		}
		seen[r] = true
	}
}

// The handler enum is closed, and parseHandler is its only door.
func TestParseHandlerAcceptsExactlyTwoValues(t *testing.T) {
	accepted := map[string]string{
		"container":    HandlerContainer,
		"host":         HandlerHost,
		"  CONTAINER ": HandlerContainer,
		"Host":         HandlerHost,
	}
	for in, want := range accepted {
		if got, ok := parseHandler(in); !ok || got != want {
			t.Errorf("parseHandler(%q) = (%q, %v), want (%q, true)", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "  ", "k8s", "kube", "containers", "hosts", "kubernetes", "docker", "podman"} {
		if got, ok := parseHandler(in); ok {
			t.Errorf("parseHandler(%q) = (%q, true), want it rejected", in, got)
		}
	}
}
