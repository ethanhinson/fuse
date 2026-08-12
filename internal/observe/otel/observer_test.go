package otel

import (
	"context"
	"errors"
	"testing"

	"github.com/ethanhinson/fuse/internal/observe"
	codes "go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

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
