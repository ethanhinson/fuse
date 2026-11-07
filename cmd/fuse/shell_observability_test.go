package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/observe"
	"go.opentelemetry.io/otel"
)

// invalidTracesConfig enables trace export with no endpoint, which validation refuses.
func invalidTracesConfig() config.Config {
	return config.Config{Observability: config.ObservabilityConfig{
		Traces: config.TracesObservabilityConfig{Enabled: true, Protocol: "not-a-protocol"},
	}}
}

// TestSetupShellObservabilityEmptyConfigIsNoop is the byte-identical-default
// regression guard for change 0061: with no observability config the shell gets
// the NoopObserver and prints nothing.
func TestSetupShellObservabilityEmptyConfigIsNoop(t *testing.T) {
	var errb bytes.Buffer
	obs, _, code, ok := setupShellObservability(context.Background(), config.Config{}, &errb)
	if !ok {
		t.Fatalf("setup failed with code %d: %s", code, errb.String())
	}
	t.Cleanup(func() { _ = obs.Close(context.Background()) })
	if _, isNoop := obs.observer.(observe.NoopObserver); !isNoop {
		t.Fatalf("empty config must yield NoopObserver, got %T", obs.observer)
	}
	if errb.Len() != 0 {
		t.Fatalf("empty config must produce no output; stderr=%q", errb.String())
	}
}

// TestSetupShellObservabilityFailsFastOnInvalidConfig covers settled decision 2:
// an invalid observability config aborts startup rather than degrading to noop.
func TestSetupShellObservabilityFailsFastOnInvalidConfig(t *testing.T) {
	cfg := invalidTracesConfig()

	var errb bytes.Buffer
	_, _, code, ok := setupShellObservability(context.Background(), cfg, &errb)
	if ok {
		t.Fatal("invalid observability config must abort startup")
	}
	if code == 0 {
		t.Fatal("invalid observability config must yield a non-zero exit code")
	}
	if !strings.Contains(errb.String(), "observability") {
		t.Fatalf("validation error must reach stderr, got %q", errb.String())
	}
}

// TestRunShellFailsFastOnInvalidObservabilityConfig proves the shell entry point
// honors the fail-fast decision before it ever reaches the TUI.
func TestRunShellFailsFastOnInvalidObservabilityConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := invalidTracesConfig()

	var out, errb bytes.Buffer
	if code := runShell(nil, cfg, model.DefaultRegistry(), &out, &errb); code == 0 {
		t.Fatalf("runShell must return non-zero; stderr=%q", errb.String())
	}
	if !strings.Contains(errb.String(), "observability") {
		t.Fatalf("validation error must reach stderr, got %q", errb.String())
	}
}

// TestInstallOTELDiagnosticsRoutesToSink asserts the SDK's global error handler
// routes to the provided sink instead of the default os.Stderr — this is what
// keeps async batch-export failures ("traces export: ... rpc timeout") off the
// TUI's shared terminal, where they would otherwise shear the alt screen.
func TestInstallOTELDiagnosticsRoutesToSink(t *testing.T) {
	// OTEL registrations are process-global; reset to discard afterward so this
	// test can't leak a sink into later tests.
	t.Cleanup(func() { installOTELDiagnostics(io.Discard) })

	var sink bytes.Buffer
	installOTELDiagnostics(&sink)

	otel.Handle(errors.New("processor export error: rpc timeout"))

	if !strings.Contains(sink.String(), "rpc timeout") {
		t.Fatalf("otel error handler did not route to the sink; got %q", sink.String())
	}
}

// TestInstallOTELDiagnosticsNilSinkDiscards guards the nil-sink fallback: it must
// not panic (a nil io.Writer would) and must swallow output silently.
func TestInstallOTELDiagnosticsNilSinkDiscards(t *testing.T) {
	t.Cleanup(func() { installOTELDiagnostics(io.Discard) })
	installOTELDiagnostics(nil)
	otel.Handle(errors.New("boom")) // must not panic
}
