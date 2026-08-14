package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/observe"
	collectortracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"
)

// traceCollector installs an OTLP/HTTP collector double and returns the channel
// of span names it observed. It is the end-to-end probe for change 0061: a span
// can only arrive here if the entry point's observer actually reached the agent
// that ran the turn.
func traceCollector(t *testing.T) (endpoint string, names chan string) {
	t.Helper()
	names = make(chan string, 64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusOK)
			return
		}
		request := &collectortracepb.ExportTraceServiceRequest{}
		if err := proto.Unmarshal(body, request); err == nil {
			for _, rs := range request.ResourceSpans {
				for _, ss := range rs.ScopeSpans {
					for _, span := range ss.Spans {
						select {
						case names <- span.Name:
						default:
						}
					}
				}
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://"), names
}

// writeTracingConfig plants a ~/.fuse/config.yml that enables OTLP tracing at
// endpoint. HOME must already point at a temp dir.
func writeTracingConfig(t *testing.T, endpoint string) {
	t.Helper()
	dir := filepath.Join(os.Getenv("HOME"), ".fuse")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "observability:\n" +
		"  traces:\n" +
		"    enabled: true\n" +
		"    endpoint: " + endpoint + "\n" +
		"    protocol: http/protobuf\n" +
		"    insecure: true\n" +
		"    batch_size: 1\n" +
		"    batch_timeout: 1ms\n" +
		"    sample_ratio: 1\n"
	if err := os.WriteFile(filepath.Join(dir, "config.yml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestOneShotEntryPointObservesTheRootTurn proves the one-shot path (main.go
// run()) builds the observe layer and threads its observer all the way into the
// root agent: with tracing enabled, the root turn's spans reach the collector.
func TestOneShotEntryPointObservesTheRootTurn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	endpoint, names := traceCollector(t)
	writeTracingConfig(t, endpoint)
	scriptedGateway(t, "ONE-SHOT-REPLY")

	var out, errb bytes.Buffer
	if code := run([]string{"--model", "deepseek-flash", "say hi"}, &out, &errb); code != 0 {
		t.Fatalf("run exit=%d stderr=%s", code, errb.String())
	}
	select {
	case <-names:
	case <-time.After(2 * time.Second):
		t.Fatal("one-shot run exported no spans; the entry-point observer never reached the root agent")
	}
}

// TestResearchProbeEntryPointObservesTheRootTurn is the same proof for the
// research-probe entry point.
func TestResearchProbeEntryPointObservesTheRootTurn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	endpoint, names := traceCollector(t)
	writeTracingConfig(t, endpoint)
	scriptedGateway(t, "PROBE-REPLY")

	var out, errb bytes.Buffer
	if code := run([]string{"research-probe", "--model", "deepseek-flash", "--timeout", "30s", "what is fuse"}, &out, &errb); code != 0 {
		t.Fatalf("run exit=%d stderr=%s", code, errb.String())
	}
	select {
	case <-names:
	case <-time.After(2 * time.Second):
		t.Fatal("research-probe run exported no spans; the entry-point observer never reached the root agent")
	}
}

// TestSetupLocalObservabilityEmptyConfigIsNoop is the shared byte-identical
// default guard for the non-shell entry points.
func TestSetupLocalObservabilityEmptyConfigIsNoop(t *testing.T) {
	var out, errb bytes.Buffer
	obs, code, ok := setupLocalObservability(context.Background(), config.Config{}, &out, &errb, "one-shot")
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

// TestSetupLocalObservabilityFailsFastOnInvalidConfig covers settled decision 2
// for the non-shell entry points, including the label they identify with.
func TestSetupLocalObservabilityFailsFastOnInvalidConfig(t *testing.T) {
	cfg := enabledMetricsShellConfig("")
	cfg.Observability.Metrics.Path = "not-absolute"

	var out, errb bytes.Buffer
	_, code, ok := setupLocalObservability(context.Background(), cfg, &out, &errb, "one-shot")
	if ok || code == 0 {
		t.Fatal("invalid observability config must abort startup with a non-zero code")
	}
	if !strings.Contains(errb.String(), "one-shot: observability") {
		t.Fatalf("labeled validation error must reach stderr, got %q", errb.String())
	}
}
