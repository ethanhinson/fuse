package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/observe"
	"github.com/ethanhinson/fuse/internal/version"
	collectortracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"
)

func TestObservabilityDisabledIsNoop(t *testing.T) {
	s, err := newObservability(context.Background(), config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if s.provider != nil {
		t.Fatal("disabled observability constructed an adapter")
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestObservabilityExplicitZeroTraceSampleRatioDropsRootSpans(t *testing.T) {
	cfg := config.Config{Observability: config.ObservabilityConfig{Traces: config.TracesObservabilityConfig{
		Enabled: true, Endpoint: "127.0.0.1:1", Protocol: "grpc", Insecure: true, SampleRatio: 0,
	}}}
	s, err := newObservability(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })

	_, span := s.provider.Tracer("test").Start(context.Background(), "root")
	defer span.End()
	if span.IsRecording() {
		t.Fatal("explicit zero trace sample ratio recorded a root span")
	}
}

func TestNewObservabilityExportsServiceResource(t *testing.T) {
	requests := make(chan *collectortracepb.ExportTraceServiceRequest, 1)
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/traces" {
			t.Errorf("path=%q, want /v1/traces", r.URL.Path)
		}
		defer r.Body.Close()
		request := &collectortracepb.ExportTraceServiceRequest{}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		if err := proto.Unmarshal(body, request); err != nil {
			t.Error(err)
			return
		}
		requests <- request
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(collector.Close)

	cfg := config.Config{Observability: config.ObservabilityConfig{
		InstanceID: "resource-test-instance",
		Traces: config.TracesObservabilityConfig{
			Enabled: true, Endpoint: strings.TrimPrefix(collector.URL, "http://"), Protocol: "http/protobuf", Insecure: true,
			BatchSize: 1, BatchTimeout: "1ms", SampleRatio: 1,
		},
	}}
	s, err := newObservability(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })

	_, span := s.observer.Start(context.Background(), observe.Descriptor{Kind: observe.OperationAPIRequest, Name: "resource"})
	span.End(observe.OutcomeSuccess)
	if err := s.provider.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}

	select {
	case request := <-requests:
		if len(request.ResourceSpans) != 1 || len(request.ResourceSpans[0].ScopeSpans) == 0 || len(request.ResourceSpans[0].ScopeSpans[0].Spans) == 0 {
			t.Fatalf("exported resource spans=%+v", request.ResourceSpans)
		}
		attributes := make(map[string]string)
		for _, attribute := range request.ResourceSpans[0].Resource.Attributes {
			attributes[attribute.Key] = attribute.Value.GetStringValue()
		}
		for key, want := range map[string]string{"service.name": "fuse", "service.version": version.Version, "service.instance.id": "resource-test-instance"} {
			if got := attributes[key]; got != want {
				t.Errorf("resource %s=%q, want %q", key, got, want)
			}
		}
	case <-time.After(time.Second):
		t.Fatal("OTLP collector did not receive an exported span")
	}
}

// --- FUSE_INSTANCE_ID resolution (docket 0076 Task 5) -----------------------
//
// The trusted config file still wins (ADR-0006: `observability` is honored from
// the trusted home file and nowhere else). The env is consulted only when the
// file leaves instance_id empty.

func TestResolveInstanceIDPrefersConfigOverEnv(t *testing.T) {
	t.Setenv("FUSE_INSTANCE_ID", "from-env")
	if got := resolveInstanceID("from-config"); got != "from-config" {
		t.Fatalf("resolveInstanceID=%q, want from-config", got)
	}
}

func TestResolveInstanceIDFallsBackToEnv(t *testing.T) {
	t.Setenv("FUSE_INSTANCE_ID", "from-env")
	if got := resolveInstanceID(""); got != "from-env" {
		t.Fatalf("resolveInstanceID=%q, want from-env", got)
	}
}

func TestResolveInstanceIDTrimsEnvWhitespace(t *testing.T) {
	t.Setenv("FUSE_INSTANCE_ID", "  padded  ")
	if got := resolveInstanceID(""); got != "padded" {
		t.Fatalf("resolveInstanceID=%q, want padded", got)
	}
}

func TestResolveInstanceIDFallsBackToHostname(t *testing.T) {
	t.Setenv("FUSE_INSTANCE_ID", "")
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		t.Skip("hostname unavailable on this machine")
	}
	if got := resolveInstanceID(""); got != hostname {
		t.Fatalf("resolveInstanceID=%q, want hostname %q", got, hostname)
	}
}

// TestObservabilityOutageDoesNotBlockOperations: a collector that hangs and a full queue
// must never hold up the work being observed.
func TestObservabilityOutageDoesNotBlockOperations(t *testing.T) {
	collector := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		time.Sleep(200 * time.Millisecond)
	}))
	t.Cleanup(collector.Close)
	cfg := config.Config{Observability: config.ObservabilityConfig{
		Traces: config.TracesObservabilityConfig{
			Enabled: true, Endpoint: strings.TrimPrefix(collector.URL, "http://"), Protocol: "http/protobuf", Insecure: true,
			QueueSize: 1, BatchSize: 1, ExportTimeout: "20ms", BatchTimeout: "1ms", SampleRatio: 1,
		},
	}}
	service, err := newObservability(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = service.Close(ctx)
	})

	start := time.Now()
	for i := 0; i < 100; i++ {
		_, handle := service.observer.Start(context.Background(), observe.Descriptor{Kind: observe.OperationLoop, Name: "run"})
		handle.End(observe.OutcomeSuccess)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("operation completion blocked on OTLP outage for %s", elapsed)
	}
}
