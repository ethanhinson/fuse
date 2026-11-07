package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/observe"
	observeotel "github.com/ethanhinson/fuse/internal/observe/otel"
	"github.com/ethanhinson/fuse/internal/version"
	"github.com/go-logr/logr/funcr"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
)

// instanceIDEnv names the instance-id fallback, consulted only when the trusted
// config file leaves observability.instance_id empty. A container per task can set
// it per run where one shared config file cannot.
const instanceIDEnv = "FUSE_INSTANCE_ID"

// resolveInstanceID applies the instance-id resolution order:
// configured (non-empty) -> $FUSE_INSTANCE_ID -> os.Hostname() -> "".
func resolveInstanceID(configured string) string {
	if id := strings.TrimSpace(configured); id != "" {
		return id
	}
	if id := strings.TrimSpace(os.Getenv(instanceIDEnv)); id != "" {
		return id
	}
	if hostname, err := os.Hostname(); err == nil {
		return strings.TrimSpace(hostname)
	}
	return ""
}

// observabilityService is the optional OTLP trace export behind the session's
// observer. With traces disabled the observer is a no-op.
type observabilityService struct {
	observer  observe.Observer
	provider  *sdktrace.TracerProvider
	closeOnce sync.Once
	closeErr  error
}

func newObservability(ctx context.Context, cfg config.Config) (*observabilityService, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	o := cfg.Observability
	s := &observabilityService{observer: observe.NoopObserver{}}
	if !o.Traces.Enabled {
		return s, nil
	}
	exporter, err := newOTLPExporter(ctx, o.Traces)
	if err != nil {
		return nil, fmt.Errorf("trace adapter: %w", err)
	}
	batch := observeotel.BatchConfig{QueueSize: o.Traces.QueueSize, BatchSize: o.Traces.BatchSize}
	if o.Traces.ExportTimeout != "" {
		batch.ExportTimeout, _ = time.ParseDuration(o.Traces.ExportTimeout)
	}
	if o.Traces.BatchTimeout != "" {
		batch.BatchTimeout, _ = time.ParseDuration(o.Traces.BatchTimeout)
	}
	resourceAttributes := []attribute.KeyValue{semconv.ServiceName("fuse"), semconv.ServiceVersion(version.Version)}
	if id := resolveInstanceID(o.InstanceID); id != "" {
		resourceAttributes = append(resourceAttributes, semconv.ServiceInstanceID(id))
	}
	s.provider = observeotel.NewProvider(exporter, batch,
		sdktrace.WithResource(resource.NewWithAttributes(semconv.SchemaURL, resourceAttributes...)),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(o.Traces.SampleRatio))),
	)
	s.observer = observeotel.New(s.provider)
	return s, nil
}

// installOTELDiagnostics routes the OpenTelemetry SDK's own diagnostic output (its
// global error handler and internal logger) to sink instead of os.Stderr.
//
// This matters for the TUI: runShell hands the terminal to bubbletea, and any async
// write to stderr from a background batch-export goroutine lands on the alt screen
// mid-render and shears the display.
//
// Both are process-global OTEL registrations; call once per process. A nil sink falls
// back to io.Discard.
func installOTELDiagnostics(sink io.Writer) {
	if sink == nil {
		sink = io.Discard
	}
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		fmt.Fprintf(sink, "otel: %v\n", err)
	}))
	otel.SetLogger(funcr.New(func(prefix, args string) {
		if prefix != "" {
			fmt.Fprintf(sink, "otel: %s: %s\n", prefix, args)
		} else {
			fmt.Fprintf(sink, "otel: %s\n", args)
		}
	}, funcr.Options{}))
}

func newOTLPExporter(ctx context.Context, c config.TracesObservabilityConfig) (sdktrace.SpanExporter, error) {
	if c.Protocol == "grpc" {
		opts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(c.Endpoint), otlptracegrpc.WithHeaders(c.Headers)}
		if c.Insecure {
			opts = append(opts, otlptracegrpc.WithInsecure())
		}
		return otlptracegrpc.New(ctx, opts...)
	}
	opts := []otlptracehttp.Option{otlptracehttp.WithEndpoint(c.Endpoint), otlptracehttp.WithHeaders(c.Headers)}
	if c.Insecure {
		opts = append(opts, otlptracehttp.WithInsecure())
	}
	return otlptracehttp.New(ctx, opts...)
}

// Close flushes and stops the trace exporter. It is safe to call more than once.
func (s *observabilityService) Close(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		if s.provider != nil {
			s.closeErr = s.provider.Shutdown(ctx)
		}
	})
	return s.closeErr
}

// setupLocalObservability builds the session-shared observer for an entry point. It
// is the only place an entry point builds one, so root and children share one
// observer. An invalid config fails startup rather than silently degrading to a noop
// observer.
//
// label prefixes the diagnostics so the operator sees which entry point spoke. The
// returned bool reports whether startup may proceed; when it is false the caller must
// return the accompanying exit code. The second return is the matching teardown,
// always non-nil, which callers defer right after the ok check.
func setupLocalObservability(ctx context.Context, cfg config.Config, stderr io.Writer, label string) (*observabilityService, func(), int, bool) {
	obs, err := newObservability(ctx, cfg)
	if err != nil {
		fmt.Fprintf(stderr, "%s: observability: %v\n", label, err)
		return nil, func() {}, 1, false
	}
	closeObs := func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if cerr := obs.Close(sctx); cerr != nil {
			fmt.Fprintf(stderr, "%s: observability shutdown: %v\n", label, cerr)
		}
	}
	return obs, closeObs, 0, true
}

// setupShellObservability is setupLocalObservability under the shell's label, keeping
// the OTEL SDK's own diagnostics off the terminal: both std streams share the TTY, so
// an async export error written to stderr shears the alt screen mid-render.
func setupShellObservability(ctx context.Context, cfg config.Config, stderr io.Writer) (*observabilityService, func(), int, bool) {
	obs, closeObs, code, ok := setupLocalObservability(ctx, cfg, stderr, "shell")
	if ok {
		installOTELDiagnostics(io.Discard)
	}
	return obs, closeObs, code, ok
}
