package main

import (
	"bytes"
	"context"
	"net"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/observe"
	observabilitylogging "github.com/ethanhinson/fuse/internal/observe/logging"
)

// enabledMetricsShellConfig is a minimal VALID observability config with the
// Prometheus scrape endpoint enabled and bound to bind.
func enabledMetricsShellConfig(bind string) config.Config {
	return config.Config{Observability: config.ObservabilityConfig{
		Metrics: config.MetricsObservabilityConfig{Enabled: true, Path: "/metrics", Access: "public", Bind: bind},
		Cardinality: config.CardinalityObservabilityConfig{
			HashVersion: "sha256-64-v1", Salt: "shell",
			Tenant: config.CardinalityDimensionConfig{Budget: 1},
			Model:  config.CardinalityDimensionConfig{Budget: 1},
			Tool:   config.CardinalityDimensionConfig{Budget: 1},
		},
	}}
}

// TestSetupShellObservabilityEmptyConfigIsNoop is the byte-identical-default
// regression guard for change 0061: with no observability config the shell gets
// the NoopObserver, opens no metrics listener, and prints nothing.
func TestSetupShellObservabilityEmptyConfigIsNoop(t *testing.T) {
	var out, errb bytes.Buffer
	obs, _, code, ok := setupShellObservability(context.Background(), config.Config{}, &out, &errb)
	if !ok {
		t.Fatalf("setup failed with code %d: %s", code, errb.String())
	}
	t.Cleanup(func() { _ = obs.Close(context.Background()) })
	if _, isNoop := obs.observer.(observe.NoopObserver); !isNoop {
		t.Fatalf("empty config must yield NoopObserver, got %T", obs.observer)
	}
	if obs.metricsListener != nil || obs.metricsServer != nil {
		t.Fatal("empty config must open no metrics listener")
	}
	if out.Len() != 0 || errb.Len() != 0 {
		t.Fatalf("empty config must produce no output; stdout=%q stderr=%q", out.String(), errb.String())
	}
}

// TestSetupShellObservabilityFailsFastOnInvalidConfig covers settled decision 2:
// an invalid observability config aborts startup rather than degrading to noop.
func TestSetupShellObservabilityFailsFastOnInvalidConfig(t *testing.T) {
	cfg := enabledMetricsShellConfig("")
	cfg.Observability.Metrics.Path = "not-absolute"

	var out, errb bytes.Buffer
	_, _, code, ok := setupShellObservability(context.Background(), cfg, &out, &errb)
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

// TestSetupShellObservabilityWarnsOnMetricsBindFailure covers settled decision 1:
// a metrics endpoint that cannot bind warns and lets the shell continue.
func TestSetupShellObservabilityWarnsOnMetricsBindFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	var out, errb bytes.Buffer
	obs, _, code, ok := setupShellObservability(context.Background(), enabledMetricsShellConfig(ln.Addr().String()), &out, &errb)
	if !ok {
		t.Fatalf("a metrics bind failure must NOT abort the shell (code %d): %s", code, errb.String())
	}
	t.Cleanup(func() { _ = obs.Close(context.Background()) })
	if !strings.Contains(errb.String(), "metrics endpoint") {
		t.Fatalf("bind failure must warn on stderr, got %q", errb.String())
	}
}

// TestRunShellFailsFastOnInvalidObservabilityConfig proves the shell entry point
// honors the fail-fast decision before it ever reaches the TUI.
func TestRunShellFailsFastOnInvalidObservabilityConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := enabledMetricsShellConfig("")
	cfg.Observability.Metrics.Path = "not-absolute"

	var out, errb bytes.Buffer
	if code := runShell(nil, cfg, model.DefaultRegistry(), &out, &errb); code == 0 {
		t.Fatalf("runShell must return non-zero; stderr=%q", errb.String())
	}
	if !strings.Contains(errb.String(), "observability") {
		t.Fatalf("validation error must reach stderr, got %q", errb.String())
	}
}

// TestSetupShellObservabilityKeepsLogSinkOffTheTUIWriter guards the writer the
// TUI owns. runShell passes stdout to tea.NewProgram(..., tea.WithOutput(stdout))
// for the whole session, so the structured JSONL logger must never be pointed at
// it -- otherwise `observability.logging.enabled: true` with a non-file output
// streams log lines straight into the alt screen. Pure wiring assertion: no
// model traffic.
func TestSetupShellObservabilityKeepsLogSinkOffTheTUIWriter(t *testing.T) {
	// "stdout" and "file" are the only outputs config.Validate accepts, and
	// "file" opens its own sink that cannot reach the TUI writer.
	for _, output := range []string{"stdout"} {
		t.Run("output="+output, func(t *testing.T) {
			var out, errb bytes.Buffer
			obs, _, code, ok := setupShellObservability(context.Background(), loggingConfig(output, ""), &out, &errb)
			if !ok {
				t.Fatalf("setup failed with code %d: %s", code, errb.String())
			}
			t.Cleanup(func() { _ = obs.Close(context.Background()) })
			if obs.logger == nil {
				t.Fatal("logging enabled but no logger was constructed")
			}
			if err := obs.logger.Audit(context.Background(), observabilitylogging.Audit{Actor: "test", Action: "probe", Scope: "logging", Outcome: "success"}); err != nil {
				t.Fatal(err)
			}
			if out.Len() != 0 {
				t.Fatalf("shell log sink wrote to the TUI writer (stdout): %q", out.String())
			}
			if !strings.Contains(errb.String(), "probe") {
				t.Fatalf("shell log sink must write to stderr, got %q", errb.String())
			}
		})
	}
}
