package otel

import (
	"context"
	"net/http"

	"github.com/ethanhinson/fuse/internal/event"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

var traceContext = propagation.TraceContext{}

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
