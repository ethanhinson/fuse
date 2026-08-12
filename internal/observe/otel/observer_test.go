package otel

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/observe"
	codes "go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

type blockingFailExporter struct {
	entered chan struct{}
	release chan struct{}
}

func (e *blockingFailExporter) ExportSpans(ctx context.Context, _ []trace.ReadOnlySpan) error {
	select {
	case e.entered <- struct{}{}:
	default:
	}
	select {
	case <-e.release:
		return errors.New("collector unavailable")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (*blockingFailExporter) Shutdown(context.Context) error { return nil }

func TestBatchProviderOutageIsBoundedAndReportsHealth(t *testing.T) {
	exporter := &blockingFailExporter{entered: make(chan struct{}, 1), release: make(chan struct{})}
	var exportErrors, drops atomic.Uint64
	provider := NewProvider(exporter, BatchConfig{
		QueueSize: 1, BatchSize: 1, BatchTimeout: time.Millisecond, ExportTimeout: 20 * time.Millisecond,
		OnExportError: func() { exportErrors.Add(1) }, OnDrop: func() { drops.Add(1) },
	})
	observer := New(provider)

	start := time.Now()
	for i := 0; i < 100; i++ {
		_, handle := observer.Start(context.Background(), observe.Descriptor{Kind: observe.OperationLoop, Name: "run"})
		handle.End(observe.OutcomeSuccess)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("span completion blocked on exporter outage for %s", elapsed)
	}

	select {
	case <-exporter.entered:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("exporter was not called")
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && (exportErrors.Load() == 0 || drops.Load() == 0) {
		time.Sleep(time.Millisecond)
	}
	if exportErrors.Load() == 0 {
		t.Fatal("export errors were not reported")
	}
	if drops.Load() == 0 {
		t.Fatal("queue saturation drops were not reported")
	}
	close(exporter.release)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = provider.Shutdown(ctx)
}

func TestObserverHierarchyAndTerminalMapping(t *testing.T) {
	ex := tracetest.NewInMemoryExporter()
	tp := trace.NewTracerProvider(trace.WithSyncer(ex))
	o := New(tp)
	ctx, api := o.Start(context.Background(), observe.Descriptor{Kind: observe.OperationAPIRequest, Name: "start", Fields: []observe.Field{{Key: "tenant", Value: "acme"}}})
	_, loop := o.Start(ctx, observe.Descriptor{Kind: observe.OperationLoop, Name: "run"})
	loop.End(observe.OutcomeError, observe.Field{Key: "error.type", Value: "model"})
	api.End(observe.OutcomeSuccess)
	spans := ex.GetSpans()
	if len(spans) != 2 {
		t.Fatalf("spans=%d, want 2", len(spans))
	}
	if spans[0].Name != "fuse.loop.run" || spans[0].Parent.SpanID() != spans[1].SpanContext.SpanID() {
		t.Fatalf("hierarchy wrong: %#v", spans)
	}
	if spans[0].Status.Code != codes.Error {
		t.Fatalf("error status=%v", spans[0].Status.Code)
	}
	if spans[1].Name != "fuse.api.request.start" {
		t.Fatalf("api name=%q", spans[1].Name)
	}
}

func TestOutcomeFromError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := OutcomeFromError(ctx, ctx.Err()); got != observe.OutcomeCanceled {
		t.Fatalf("got %q", got)
	}
	if got := OutcomeFromError(context.Background(), context.DeadlineExceeded); got != observe.OutcomeTimeout {
		t.Fatalf("got %q", got)
	}
	if got := OutcomeFromError(context.Background(), errors.New("boom")); got != observe.OutcomeError {
		t.Fatalf("got %q", got)
	}
	if got := OutcomeFromError(context.Background(), nil); got != observe.OutcomeSuccess {
		t.Fatalf("got %q", got)
	}
}
