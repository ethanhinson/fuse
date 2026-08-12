package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/observe"
	observeotel "github.com/ethanhinson/fuse/internal/observe/otel"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func acceptanceObservabilityConfig() config.Config {
	return config.Config{Observability: config.ObservabilityConfig{
		InstanceID: "acceptance-instance",
		Metrics:    config.MetricsObservabilityConfig{Enabled: true, Path: "/metrics", Access: "public"},
		Logging:    config.LoggingObservabilityConfig{Enabled: true, Output: "stdout", Level: "debug", MaxOverrideTTL: "1m"},
		Cardinality: config.CardinalityObservabilityConfig{
			HashVersion: "sha256-64-v1", Salt: "acceptance",
			Tenant: config.CardinalityDimensionConfig{Budget: 1, Catalog: []string{"tenant-a"}},
			Model:  config.CardinalityDimensionConfig{Budget: 1, Catalog: []string{"model-a"}},
			Tool:   config.CardinalityDimensionConfig{Budget: 1, Catalog: []string{"tool-a"}},
		},
	}}
}

// This is the hermetic observability acceptance lane: it exercises the production
// fanout, Prometheus handler, payload-free JSON logger, and OTEL observer together.
// Connect/runtime/SDK identity and replay are permanently covered by the adjacent
// TestAuth* acceptance harness; keeping exporters in-memory makes this lane reliable.
func TestObservabilityAcceptanceHermetic(t *testing.T) {
	var logs bytes.Buffer
	service, err := newObservability(context.Background(), acceptanceObservabilityConfig(), &logs)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close(context.Background()) })

	exporter := tracetest.NewInMemoryExporter()
	provider := observeotel.NewProvider(exporter, observeotel.BatchConfig{BatchSize: 1, BatchTimeout: time.Millisecond})
	service.provider = provider
	service.observer = observeotel.New(provider)

	ctx, root := service.observer.Start(context.Background(), observe.Descriptor{Kind: observe.OperationAPIRequest, Name: "start"})
	ctx, loop := service.observer.Start(ctx, observe.Descriptor{Kind: observe.OperationLoop, Name: "run"})
	_, model := service.observer.Start(ctx, observe.Descriptor{Kind: observe.OperationModelAttempt, Name: "complete"})
	model.End(observe.OutcomeSuccess)
	loop.End(observe.OutcomeSuccess)
	root.End(observe.OutcomeSuccess)

	key := event.StreamKey{Tenant: "tenant-a", Loop: "loop-a"}
	secret := "PROMPT-and-tool-arguments-must-not-leak"
	for i, kind := range []event.Kind{event.KindTurnStart, event.KindModelCallStart, event.KindModelCallEnd, event.KindToolCall, event.KindToolResult, event.KindSpawnStart, event.KindSpawnDone, event.KindTurnEnd} {
		e := event.Event{Seq: event.Seq(i + 1), TS: time.Now().UTC(), Kind: kind, NodeID: "node-a", Payload: json.RawMessage(`{"content":"` + secret + `"}`)}
		if err := service.projector.Project(ctx, observe.ProjectEvent(key, e)); err != nil {
			t.Fatal(err)
		}
	}
	if err := provider.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	spans := exporter.GetSpans()
	byName := make(map[string]tracetest.SpanStub, len(spans))
	for _, span := range spans {
		byName[span.Name] = span
	}
	api, apiOK := byName["fuse.api.request.start"]
	loopSpan, loopOK := byName["fuse.loop.run"]
	modelSpan, modelOK := byName["fuse.model.attempt.complete"]
	if len(spans) != 3 || !apiOK || !loopOK || !modelOK || loopSpan.Parent.SpanID() != api.SpanContext.SpanID() || modelSpan.Parent.SpanID() != loopSpan.SpanContext.SpanID() {
		t.Fatalf("unexpected nested spans: %#v", spans)
	}
	if strings.Contains(logs.String(), secret) {
		t.Fatal("structured logs leaked event payload")
	}
	if !strings.Contains(logs.String(), `"tenant_id":"tenant-a"`) || !strings.Contains(logs.String(), `"loop_id":"loop-a"`) {
		t.Fatal("logs lack tenant/loop correlation")
	}

	w := httptest.NewRecorder()
	service.metrics.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	body := w.Body.String()
	for _, metric := range []string{"fuse_event_projection_total", "fuse_loop_operations_total", "fuse_tool_calls_total", "fuse_spawn_operations_total"} {
		if !strings.Contains(body, metric) {
			t.Errorf("metrics exposition missing %s", metric)
		}
	}
}

type failingProjector struct{ err error }

func (p failingProjector) Project(context.Context, observe.Record) error { return p.err }

func TestObservationOutageIsBoundedAndDoesNotFailLoopAppend(t *testing.T) {
	store := projectingDurableStore{DurableStore: outageDurableStore{}, projector: failingProjector{err: errors.New("projector unavailable")}}
	start := time.Now()
	if err := store.Append(context.Background(), event.StreamKey{Tenant: "tenant-a", Loop: "loop-a"}, event.Event{Kind: event.KindTurnStart}); err != nil {
		t.Fatalf("telemetry outage reached loop: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("projection outage blocked append for %s", elapsed)
	}
}

type outageDurableStore struct{}

func (outageDurableStore) Append(context.Context, event.StreamKey, event.Event) error { return nil }
func (outageDurableStore) Subscribe(context.Context, event.StreamKey) (<-chan event.Event, func(), error) {
	ch := make(chan event.Event)
	return ch, func() { close(ch) }, nil
}
func (outageDurableStore) Replay(context.Context, event.StreamKey, event.Seq) ([]event.Event, error) {
	return nil, nil
}

func TestObservabilityPermanentTargetsAndCI(t *testing.T) {
	makefile, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	workflow, err := os.ReadFile("../../.github/workflows/integration.yml")
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"observability-acceptance:", "observability-race:", "observability-compose-smoke:"} {
		if !bytes.Contains(makefile, []byte(target)) {
			t.Errorf("Makefile missing %s", target)
		}
	}
	if !bytes.Contains(workflow, []byte("observability-acceptance")) || !bytes.Contains(workflow, []byte("make observability-race")) {
		t.Fatal("CI missing permanent observability acceptance/race lane")
	}
	if bytes.Contains(workflow, []byte("observability-compose-smoke")) {
		t.Fatal("external Compose smoke must remain opt-in")
	}
}
