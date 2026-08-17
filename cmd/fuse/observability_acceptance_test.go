package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/event"
	loopv1 "github.com/ethanhinson/fuse/internal/loopwire/v1"
	"github.com/ethanhinson/fuse/internal/observe"
	observeotel "github.com/ethanhinson/fuse/internal/observe/otel"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// TestObservabilityAcceptanceHermetic is the permanent, hermetic acceptance lane.
// It invokes the direct `fuse loop-serve-net` CLI entrypoint, then drives the real
// Connect server through the generated SDK and a local scripted gateway. The only
// replacement is the OTEL exporter, which stays in memory so no external provider
// or Compose stack is needed in CI.
func TestObservabilityAcceptanceHermetic(t *testing.T) {
	var logs lockedBuffer
	exporter := tracetest.NewInMemoryExporter()
	provider := observeotel.NewProvider(exporter, observeotel.BatchConfig{BatchSize: 1, BatchTimeout: time.Millisecond})
	secret := "PROMPT-and-tool-arguments-must-not-leak"
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"done"}}]}`))
	}))
	t.Cleanup(gateway.Close)

	home := t.TempDir()
	t.Setenv("HOME", home)
	// Pin the loop to THIS test's scripted gateway. config.Load applies
	// LLM_GATEWAY_URL / LLM_GATEWAY_KEY as overrides ON TOP of config.yml
	// (loader.go), so a developer machine that exports them (e.g. a local LiteLLM
	// gateway) would otherwise redirect the loop to a REAL provider — producing a
	// real model turn with real tool calls whose extra spans fail this test's
	// exact-span assertion. Point the env overrides at the scripted gateway so the
	// test is hermetic regardless of the ambient shell (project policy: tests never
	// hit a real provider).
	t.Setenv("LLM_GATEWAY_URL", gateway.URL)
	t.Setenv("LLM_GATEWAY_KEY", "test-key")
	if err := os.Mkdir(filepath.Join(home, ".fuse"), 0o755); err != nil {
		t.Fatal(err)
	}
	configYAML := fmt.Sprintf("gateway:\n  url: %q\n  key: test-key\nloop_server:\n  auth:\n    - token: acceptance-token\n      tenant: tenant-a\n      subject: acceptance\nobservability:\n  instance_id: acceptance-instance\n  metrics:\n    enabled: true\n    path: /metrics\n    access: public\n  logging:\n    enabled: true\n    output: stdout\n    level: debug\n    max_override_ttl: 1m\n  cardinality:\n    hash_version: sha256-64-v1\n    salt: acceptance\n    tenant:\n      budget: 2\n      catalog: [tenant-a]\n    model:\n      budget: 2\n    tool:\n      budget: 2\n", gateway.URL)
	if err := os.WriteFile(filepath.Join(home, ".fuse", "config.yml"), []byte(configYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	oldListen := netListen
	netListen = func(string, string) (net.Listener, error) { return ln, nil }
	t.Cleanup(func() { netListen = oldListen })
	ctx, cancel := context.WithCancel(context.Background())
	oldContext := serveNetContext
	serveNetContext = func() (context.Context, context.CancelFunc) { return ctx, func() {} }
	t.Cleanup(func() { serveNetContext = oldContext; cancel() })
	oldObservability := newLoopServeNetObservability
	newLoopServeNetObservability = func(ctx context.Context, cfg config.Config, stdout io.Writer) (*observabilityService, error) {
		service, err := newObservability(ctx, cfg, stdout)
		if err != nil {
			return nil, err
		}
		service.provider = provider
		service.observer = metricsObserver{primary: observeotel.New(provider), metrics: service.metrics}
		return service, nil
	}
	t.Cleanup(func() { newLoopServeNetObservability = oldObservability })

	commandDone := make(chan int, 1)
	go func() {
		commandDone <- run([]string{"loop-serve-net", "--addr", ln.Addr().String()}, &logs, &bytes.Buffer{})
	}()
	client := bearerClient("http://"+ln.Addr().String(), "acceptance-token")
	const traceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	var started *connect.Response[loopv1.StartLoopResponse]
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		request := connect.NewRequest(&loopv1.StartLoopRequest{Task: secret})
		request.Header().Set("traceparent", traceparent)
		started, err = client.StartLoop(context.Background(), request)
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("SDK StartLoop through fuse CLI: %v", err)
	}
	if got := started.Header().Get("Fuse-Trace-Id"); got != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("StartLoop trace propagation = %q, want inbound trace ID", got)
	}
	waitForTurnEndByObserve(t, client, started.Msg.LoopId)
	if err := provider.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	spans := exporter.GetSpans()
	byName := make(map[string]tracetest.SpanStub, len(spans))
	for _, span := range spans {
		byName[span.Name] = span
	}
	api, apiOK := byName["fuse.api.request.start_loop"]
	loopSpan, loopOK := byName["fuse.loop.run"]
	modelSpan, modelOK := byName["fuse.model.attempt.complete"]
	if !apiOK || !loopOK || !modelOK || loopSpan.Parent.SpanID() != api.SpanContext.SpanID() || modelSpan.Parent.SpanID() != loopSpan.SpanContext.SpanID() {
		t.Fatalf("unexpected nested spans: %#v", spans)
	}
	logDeadline := time.Now().Add(time.Second)
	for time.Now().Before(logDeadline) && (!strings.Contains(logs.String(), `"tenant_id":"tenant-a"`) || !strings.Contains(logs.String(), `"loop_id":"`)) {
		time.Sleep(time.Millisecond)
	}
	if !strings.Contains(logs.String(), `"tenant_id":"tenant-a"`) || !strings.Contains(logs.String(), `"loop_id":"`) {
		t.Fatalf("logs lack tenant/loop correlation: %s", logs.String())
	}
	if strings.Contains(logs.String(), secret) {
		t.Fatal("structured logs leaked event payload")
	}

	response, err := http.Get("http://" + ln.Addr().String() + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	bodyBytes, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	body := string(bodyBytes)
	for _, metric := range []string{"fuse_event_projection_total", "fuse_loop_operations_total", "fuse_loop_operation_duration_seconds", "fuse_model_calls_total", "fuse_model_call_attempts"} {
		if !strings.Contains(body, metric) {
			t.Errorf("metrics exposition missing %s", metric)
		}
	}
	cancel()
	if code := <-commandDone; code != 0 {
		t.Fatalf("fuse loop-serve-net exit = %d, want 0", code)
	}
}

type failingProjector struct{ err error }

func (p failingProjector) Project(context.Context, observe.Record) error { return p.err }

func TestObservationOutageIsBoundedAndDoesNotFailLoopAppend(t *testing.T) {
	dispatcher := newProjectionDispatcher(failingProjector{err: errors.New("projector unavailable")}, 1)
	store := projectingDurableStore{CommittedDurableStore: outageDurableStore{}, projection: dispatcher}
	start := time.Now()
	if err := store.Append(context.Background(), event.StreamKey{Tenant: "tenant-a", Loop: "loop-a"}, event.Event{Kind: event.KindTurnStart}); err != nil {
		t.Fatalf("telemetry outage reached loop: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("projection outage blocked append for %s", elapsed)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := dispatcher.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if got := dispatcher.Stats().Errors; got != 1 {
		t.Fatalf("projection errors = %d, want 1", got)
	}
}

type blockingProjector struct {
	entered chan struct{}
	exited  chan struct{}
	calls   *atomic.Uint64
}

func (p blockingProjector) Project(ctx context.Context, _ observe.Record) error {
	p.calls.Add(1)
	close(p.entered)
	<-ctx.Done()
	close(p.exited)
	return ctx.Err()
}

func TestBlockingProjectionCannotBlockLoopAppend(t *testing.T) {
	var projectionCalls atomic.Uint64
	projector := blockingProjector{entered: make(chan struct{}), exited: make(chan struct{}), calls: &projectionCalls}
	dispatcher := newProjectionDispatcher(projector, 1)
	store := projectingDurableStore{CommittedDurableStore: outageDurableStore{}, projection: dispatcher}
	key := event.StreamKey{Tenant: "tenant-a", Loop: "loop-a"}
	if err := store.Append(context.Background(), key, event.Event{Kind: event.KindTurnStart}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-projector.entered:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("projector did not enter blocking sink")
	}
	start := time.Now()
	if err := store.Append(context.Background(), key, event.Event{Kind: event.KindTurnEnd}); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(context.Background(), key, event.Event{Kind: event.KindError}); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("blocking projection blocked durable append for %s", elapsed)
	}
	if got := dispatcher.Stats().Dropped; got != 1 {
		t.Fatalf("dropped projections = %d, want 1", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := dispatcher.Close(ctx); err != nil {
		t.Fatalf("blocked dispatcher Close error = %v", err)
	}
	select {
	case <-projector.exited:
	default:
		t.Fatal("dispatcher returned before the blocking projector exited")
	}
	if err := dispatcher.Close(ctx); err != nil {
		t.Fatalf("second dispatcher Close error = %v", err)
	}
	if got := projectionCalls.Load(); got != 1 {
		t.Fatalf("blocking projector calls = %d, want 1", got)
	}
}

type outageDurableStore struct{}

func (outageDurableStore) Append(context.Context, event.StreamKey, event.Event) error { return nil }
func (outageDurableStore) AppendCommitted(_ context.Context, _ event.StreamKey, e event.Event) (event.Event, error) {
	return e, nil
}
func (outageDurableStore) Subscribe(context.Context, event.StreamKey) (<-chan event.Event, func(), error) {
	ch := make(chan event.Event)
	return ch, func() { close(ch) }, nil
}
func (outageDurableStore) Replay(context.Context, event.StreamKey, event.Seq) ([]event.Event, error) {
	return nil, nil
}

type recordingProjector struct{ records []observe.Record }

func (p *recordingProjector) Project(_ context.Context, record observe.Record) error {
	p.records = append(p.records, record)
	return nil
}

type committedEnvelopeStore struct{}

func (committedEnvelopeStore) Append(ctx context.Context, key event.StreamKey, e event.Event) error {
	_, err := (committedEnvelopeStore{}).AppendCommitted(ctx, key, e)
	return err
}
func (committedEnvelopeStore) AppendCommitted(_ context.Context, _ event.StreamKey, e event.Event) (event.Event, error) {
	e.Seq = 41
	e.TS = time.Date(2026, 8, 12, 1, 2, 3, 0, time.UTC)
	return e, nil
}
func (committedEnvelopeStore) Subscribe(context.Context, event.StreamKey) (<-chan event.Event, func(), error) {
	ch := make(chan event.Event)
	return ch, func() { close(ch) }, nil
}
func (committedEnvelopeStore) Replay(context.Context, event.StreamKey, event.Seq) ([]event.Event, error) {
	return nil, nil
}

func TestProjectionUsesCommittedDurableEnvelope(t *testing.T) {
	projector := &recordingProjector{}
	dispatcher := newProjectionDispatcher(projector, 1)
	store := projectingDurableStore{CommittedDurableStore: committedEnvelopeStore{}, projection: dispatcher}
	key := event.StreamKey{Tenant: "tenant-a", Loop: "loop-a"}
	if err := store.Append(context.Background(), key, event.Event{Kind: event.KindTurnStart, Trace: &event.TraceCarrier{TraceParent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", TraceState: "acme=tenant-sensitive-state"}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := dispatcher.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if len(projector.records) != 1 {
		t.Fatalf("projected %d records, want exactly one", len(projector.records))
	}
	record := projector.records[0]
	if record.Sequence != 41 || !record.Timestamp.Equal(time.Date(2026, 8, 12, 1, 2, 3, 0, time.UTC)) {
		t.Fatalf("projection used uncommitted envelope: %#v", record)
	}
	if record.TenantID != "tenant-a" || record.LoopID != "loop-a" {
		t.Fatalf("projection lost stream identity: %#v", record)
	}
	if record.TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" || record.SpanID != "00f067aa0ba902b7" {
		t.Fatalf("projection lost committed trace correlation: %#v", record)
	}
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
