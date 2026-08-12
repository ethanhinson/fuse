// Package otel adapts Fuse's provider-neutral observation contracts to OpenTelemetry.
package otel

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/observe"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

const instrumentationName = "github.com/ethanhinson/fuse"

type Observer struct{ tracer trace.Tracer }

func New(provider trace.TracerProvider) *Observer {
	if provider == nil {
		provider = trace.NewNoopTracerProvider()
	}
	return &Observer{tracer: provider.Tracer(instrumentationName)}
}

// TraceCarrier returns the portable, validated subset of the active span context.
func (o *Observer) TraceCarrier(ctx context.Context) *event.TraceCarrier {
	return CarrierFromContext(ctx)
}

// StartFromCarrier continues immediately-consumed work, while delayed/replayed
// work receives a new root trace with a causal link to the durable producer.
func (o *Observer) StartFromCarrier(ctx context.Context, c *event.TraceCarrier, delayed bool, d observe.Descriptor) (context.Context, observe.Handle) {
	parent, ok := ContextFromCarrier(ctx, c, false)
	if !ok {
		return o.Start(ctx, d)
	}
	if !delayed {
		return o.Start(parent, d)
	}
	linked := trace.SpanContextFromContext(parent)
	ctx, span := o.tracer.Start(ctx, "fuse."+string(d.Kind)+"."+d.Name, trace.WithNewRoot(), trace.WithLinks(trace.Link{SpanContext: linked}), trace.WithAttributes(fields(d.Fields)...))
	return ctx, &handle{span: span}
}

func (o *Observer) Start(ctx context.Context, d observe.Descriptor) (context.Context, observe.Handle) {
	if o == nil || o.tracer == nil {
		return ctx, observe.NoopHandle{}
	}
	attrs := fields(d.Fields)
	ctx, span := o.tracer.Start(ctx, "fuse."+string(d.Kind)+"."+d.Name, trace.WithAttributes(attrs...))
	return ctx, &handle{span: span}
}

type handle struct {
	once sync.Once
	span trace.Span
}

func (h *handle) End(outcome observe.Outcome, fs ...observe.Field) {
	if h == nil {
		return
	}
	h.once.Do(func() {
		h.span.SetAttributes(append(fields(fs), attribute.String("fuse.outcome", string(outcome)))...)
		if outcome != observe.OutcomeSuccess {
			h.span.SetStatus(codes.Error, string(outcome))
		}
		h.span.End()
	})
}

func fields(fs []observe.Field) []attribute.KeyValue {
	if len(fs) > 32 {
		fs = fs[:32]
	}
	out := make([]attribute.KeyValue, 0, len(fs))
	for _, f := range fs {
		if f.Key == "" {
			continue
		}
		v := f.Value
		if len(v) > 256 {
			v = v[:256]
		}
		out = append(out, attribute.String(f.Key, v))
	}
	return out
}

func OutcomeFromError(ctx context.Context, err error) observe.Outcome {
	if err == nil {
		return observe.OutcomeSuccess
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return observe.OutcomeTimeout
	}
	if errors.Is(err, context.Canceled) || (ctx != nil && errors.Is(ctx.Err(), context.Canceled)) {
		return observe.OutcomeCanceled
	}
	return observe.OutcomeError
}

// BatchConfig keeps exporter backpressure finite. Zero values use conservative bounds.
type BatchConfig struct {
	QueueSize, BatchSize        int
	ExportTimeout, BatchTimeout time.Duration
	OnExportError, OnDrop       func()
}

func NewProvider(exporter sdktrace.SpanExporter, cfg BatchConfig, opts ...sdktrace.TracerProviderOption) *sdktrace.TracerProvider {
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 2048
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 512
	}
	if cfg.ExportTimeout <= 0 {
		cfg.ExportTimeout = 5 * time.Second
	}
	if cfg.BatchTimeout <= 0 {
		cfg.BatchTimeout = time.Second
	}
	processor := &healthBatchProcessor{maxQueue: int64(cfg.QueueSize), onDrop: cfg.OnDrop}
	trackedExporter := healthExporter{SpanExporter: exporter, pending: &processor.pending, onError: cfg.OnExportError}
	processor.delegate = sdktrace.NewBatchSpanProcessor(trackedExporter, sdktrace.WithMaxQueueSize(cfg.QueueSize), sdktrace.WithMaxExportBatchSize(cfg.BatchSize), sdktrace.WithExportTimeout(cfg.ExportTimeout), sdktrace.WithBatchTimeout(cfg.BatchTimeout))
	bp := processor
	return sdktrace.NewTracerProvider(append(opts, sdktrace.WithSpanProcessor(bp))...)
}

// healthBatchProcessor makes the SDK batcher's non-blocking queue admission
// observable. The SDK deliberately drops on saturation without an error hook;
// owning admission here preserves that behavior while accounting for the drop.
type healthBatchProcessor struct {
	delegate sdktrace.SpanProcessor
	maxQueue int64
	pending  atomic.Int64
	onDrop   func()
}

func (p *healthBatchProcessor) OnStart(ctx context.Context, span sdktrace.ReadWriteSpan) {
	p.delegate.OnStart(ctx, span)
}

func (p *healthBatchProcessor) OnEnd(span sdktrace.ReadOnlySpan) {
	if p.pending.Add(1) > p.maxQueue {
		p.pending.Add(-1)
		if p.onDrop != nil {
			p.onDrop()
		}
		return
	}
	p.delegate.OnEnd(span)
}

func (p *healthBatchProcessor) Shutdown(ctx context.Context) error { return p.delegate.Shutdown(ctx) }
func (p *healthBatchProcessor) ForceFlush(ctx context.Context) error {
	return p.delegate.ForceFlush(ctx)
}

type healthExporter struct {
	sdktrace.SpanExporter
	pending *atomic.Int64
	onError func()
}

func (e healthExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	e.pending.Add(-int64(len(spans)))
	err := e.SpanExporter.ExportSpans(ctx, spans)
	if err != nil && e.onError != nil {
		e.onError()
	}
	return err
}
