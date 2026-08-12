package otel

import (
	"context"
	"net/http"

	"github.com/ethanhinson/fuse/internal/event"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

var traceContext = propagation.TraceContext{}

// ExtractTrusted accepts only valid W3C trace context and deliberately drops baggage.
func ExtractTrusted(ctx context.Context, header http.Header) (context.Context, *event.TraceCarrier) {
	clean := baggage.ContextWithoutBaggage(ctx)
	extracted := traceContext.Extract(clean, propagation.HeaderCarrier(header))
	sc := trace.SpanContextFromContext(extracted)
	if !sc.IsValid() || !sc.IsRemote() {
		return clean, nil
	}
	carrier := CarrierFromContext(extracted)
	if carrier == nil || carrier.Validate() != nil {
		return clean, nil
	}
	return extracted, carrier
}

func Inject(ctx context.Context, header http.Header) {
	traceContext.Inject(ctx, propagation.HeaderCarrier(header))
}

func CarrierFromContext(ctx context.Context) *event.TraceCarrier {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return nil
	}
	h := http.Header{}
	traceContext.Inject(ctx, propagation.HeaderCarrier(h))
	c := &event.TraceCarrier{TraceParent: h.Get("traceparent"), TraceState: h.Get("tracestate")}
	if c.Validate() != nil {
		return nil
	}
	return c
}

// ContextFromCarrier restores durable context. A replay starts a root context; callers
// can use the returned remote SpanContext as a link. Immediate work continues it.
func ContextFromCarrier(ctx context.Context, c *event.TraceCarrier, replay bool) (context.Context, bool) {
	if c == nil || c.Validate() != nil {
		return ctx, false
	}
	h := http.Header{"Traceparent": []string{c.TraceParent}}
	if c.TraceState != "" {
		h.Set("Tracestate", c.TraceState)
	}
	extracted := traceContext.Extract(baggage.ContextWithoutBaggage(ctx), propagation.HeaderCarrier(h))
	if !trace.SpanContextFromContext(extracted).IsValid() {
		return ctx, false
	}
	if replay {
		return trace.ContextWithRemoteSpanContext(ctx, trace.SpanContextFromContext(extracted)), true
	}
	return extracted, true
}

func TraceID(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return ""
	}
	return sc.TraceID().String()
}
