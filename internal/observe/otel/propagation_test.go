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

// TestDelayedStartWithoutCarrierIsStillANewRoot pins the half of the delayed contract
// that has no producer to link to. Delayed work is work whose logical parent has
// already ended and been exported; when no carrier survives to link it, the honest
// result is an UNLINKED ROOT — never a child of whatever span the caller happened to be
// inside, which would silently attach a long-lived unit of work to a transient request
// and defeat the reason for starting it delayed at all.
func TestDelayedStartWithoutCarrierIsStillANewRoot(t *testing.T) {
	ex := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(ex))
	o := New(tp)
	reqCtx, req := o.Start(context.Background(), observe.Descriptor{Kind: observe.OperationAPIRequest, Name: "resume"})

	_, delayed := o.StartFromCarrier(reqCtx, nil, true, observe.Descriptor{Kind: observe.OperationLoop, Name: "turn"})
	delayed.End(observe.OutcomeSuccess)
	// The immediate (delayed=false) path is unchanged: with no carrier to restore it
	// still continues the caller's trace as a child.
	_, immediate := o.StartFromCarrier(reqCtx, nil, false, observe.Descriptor{Kind: observe.OperationStore, Name: "append"})
	immediate.End(observe.OutcomeSuccess)
	req.End(observe.OutcomeSuccess)

	var turn, appended, request tracetest.SpanStub
	for _, s := range ex.GetSpans() {
		switch s.Name {
		case "fuse.loop.turn":
			turn = s
		case "fuse.store.append":
			appended = s
		case "fuse.api.request.resume":
			request = s
		}
	}
	if turn.Name == "" || request.Name == "" || appended.Name == "" {
		t.Fatalf("missing spans: %#v", ex.GetSpans())
	}
	if turn.Parent.IsValid() {
		t.Errorf("delayed span without a carrier is not a root: parent = %v", turn.Parent)
	}
	if turn.SpanContext.TraceID() == request.SpanContext.TraceID() {
		t.Error("delayed span without a carrier joined the caller's trace")
	}
	if len(turn.Links) != 0 {
		t.Errorf("delayed span without a carrier carries links %v, want none", turn.Links)
	}
	if appended.Parent.SpanID() != request.SpanContext.SpanID() {
		t.Errorf("immediate span parent = %v, want the caller's span %v (unchanged behavior)", appended.Parent, request.SpanContext)
	}
}
