package otel

import (
	"context"
	"net/http"
	"testing"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/observe"
	gootel "go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"
)

func TestExtractTrustedDropsBaggageAndRejectsMalformed(t *testing.T) {
	h := http.Header{"Traceparent": []string{"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"}, "Baggage": []string{"secret=value"}}
	ctx, carrier := ExtractTrusted(context.Background(), h)
	if carrier == nil || carrier.TraceParent != h.Get("Traceparent") {
		t.Fatalf("carrier=%#v", carrier)
	}
	if !oteltrace.SpanContextFromContext(ctx).IsRemote() {
		t.Fatal("context is not remote")
	}
	if baggage.FromContext(ctx).Len() != 0 {
		t.Fatal("baggage was retained")
	}
	if _, got := ExtractTrusted(context.Background(), http.Header{"Traceparent": []string{"bad"}}); got != nil {
		t.Fatalf("malformed accepted: %#v", got)
	}
}

func TestInjectAndRestoreCarrier(t *testing.T) {
	old := gootel.GetTextMapPropagator()
	gootel.SetTextMapPropagator(propagation.TraceContext{})
	defer gootel.SetTextMapPropagator(old)
	tp := sdktrace.NewTracerProvider()
	defer tp.Shutdown(context.Background())
	ctx, span := tp.Tracer("test").Start(context.Background(), "root")
	defer span.End()
	h := http.Header{}
	Inject(ctx, h)
	carrier := CarrierFromContext(ctx)
	if carrier == nil || h.Get("traceparent") == "" {
		t.Fatal("trace context not injected")
	}
	restored, ok := ContextFromCarrier(context.Background(), &event.TraceCarrier{TraceParent: carrier.TraceParent, TraceState: carrier.TraceState}, false)
	if !ok || oteltrace.SpanContextFromContext(restored).TraceID() != oteltrace.SpanContextFromContext(ctx).TraceID() {
		t.Fatal("carrier did not restore")
	}
}

func TestDelayedCarrierStartsNewTraceWithLink(t *testing.T) {
	ex := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(ex))
	o := New(tp)
	parentCtx, parent := tp.Tracer("test").Start(context.Background(), "parent")
	c := CarrierFromContext(parentCtx)
	_, child := o.StartFromCarrier(context.Background(), c, true, observe.Descriptor{Kind: observe.OperationStore, Name: "replay"})
	child.End(observe.OutcomeSuccess)
	parent.End()
	spans := ex.GetSpans()
	if len(spans) != 2 {
		t.Fatalf("spans=%d", len(spans))
	}
	if spans[0].SpanContext.TraceID() == spans[1].SpanContext.TraceID() {
		t.Fatal("replay continued parent trace")
	}
	if len(spans[0].Links) != 1 || spans[0].Links[0].SpanContext.TraceID() != spans[1].SpanContext.TraceID() {
		t.Fatal("replay link missing")
	}
}
