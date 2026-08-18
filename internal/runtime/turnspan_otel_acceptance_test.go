package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/model"
	observeotel "github.com/ethanhinson/fuse/internal/observe/otel"
	"github.com/ethanhinson/fuse/internal/tools"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// TestTurnScopedTraceTopologyThroughRealOTELExporter is change 0071's ACCEPTANCE
// CRITERION 1, made durable. The criterion is stated as a live observation ("while
// the session is still parked and alive, the first-turn trace exists rooted at
// fuse.loop.run and ended, and one complete fuse.loop.turn-rooted trace exists per
// completed later turn"), and every other test in this package asserts it against a
// test-double observer — which can only prove the runtime CALLED the contract, never
// that the real OTEL SDK EXPORTED the spans, with the topology, while the session
// lived.
//
// So this test replaces exactly one thing relative to production: the span EXPORTER
// (in-memory instead of OTLP/Tempo). The observer is the real internal/observe/otel
// Observer over a real sdktrace provider, and the path is the real
// StartLoop/Send/park path. The model is a scripted completer, not a gateway call:
// span topology is independent of what the model says, and the project rule is that
// tests never reach a real provider (the live-gateway half of the criterion is
// recorded in the change's results file, never in the suite).
//
// What "exported while alive" means here: every assertion below runs BEFORE the loop
// is reaped, and the test fails if the loop handle is already done.
func TestTurnScopedTraceTopologyThroughRealOTELExporter(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	// BatchSize 1 + a millisecond batch timeout keeps ForceFlush cheap; the flush
	// below is what makes the read deterministic regardless.
	provider := observeotel.NewProvider(exporter, observeotel.BatchConfig{BatchSize: 1, BatchTimeout: time.Millisecond},
		sdktrace.WithSampler(sdktrace.AlwaysSample()))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

	fake := newGatedCompleter(
		model.CompletionResp{Content: "first answer"},
		model.CompletionResp{Content: "second answer"},
		model.CompletionResp{Content: "third answer"},
	)
	rt := New(Deps{
		BaseDir:       t.TempDir(),
		MaxConcurrent: 1,
		Observer:      observeotel.New(provider),
		// A long idle TTL: this test asserts the span topology while the session is
		// STILL ALIVE, so the reaper must not be able to end it mid-assertion.
		IdleTTL: time.Minute,
		BuildAgent: func(store event.EventStore, tree *agent.AgentTree, modelID string, reg *tools.Registry) (*agent.Agent, agent.ChildBuilder, string, error) {
			return agent.New(fake, execAll{reg}, nopRenderer{}, modelID, "", 1, 0), nil, modelID, nil
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h, err := rt.StartLoop(ctx, LoopConfig{Task: "find me a rental", ModelID: "cloud/x", Interactive: true})
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	loopID := h.ID()
	evCh, unsub, err := rt.Observe(ctx, event.DefaultTenant, loopID)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	defer unsub()

	fake.admit(0)
	waitForKind(t, evCh, event.KindLoopParked, 3*time.Second)

	// --- Criterion 1a: the first-turn trace is rooted at fuse.loop.run and is
	// ENDED (hence exported) while the session is parked and alive. ---
	requireAlive(t, h, "after first park")
	flush(t, provider)
	runSpans := exportedNamed(t, exporter, "fuse.loop.run")
	if len(runSpans) != 1 {
		t.Fatalf("exported fuse.loop.run spans = %d, want 1 (ended at the first park)", len(runSpans))
	}
	sessionRoot := runSpans[0]
	if sessionRoot.Parent.IsValid() {
		t.Fatalf("fuse.loop.run is not a trace root: parent = %v", sessionRoot.Parent)
	}
	if got := attr(sessionRoot, "fuse.outcome"); got != "success" {
		t.Fatalf("fuse.loop.run outcome = %q, want success", got)
	}
	if len(exportedNamed(t, exporter, "fuse.loop.turn")) != 0 {
		t.Fatalf("a turn root was exported before any later turn ran")
	}

	// --- Criterion 1b: one complete fuse.loop.turn-rooted trace per completed
	// later turn, each linked back to the session root. ---
	fake.admit(1)
	if err := rt.Send(ctx, event.DefaultTenant, loopID, "what about Aspen?"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitForKind(t, evCh, event.KindLoopParked, 3*time.Second)
	requireAlive(t, h, "after second park")
	flush(t, provider)
	turns := exportedNamed(t, exporter, "fuse.loop.turn")
	if len(turns) != 1 {
		t.Fatalf("exported fuse.loop.turn spans after one later turn = %d, want 1", len(turns))
	}
	assertTurnRoot(t, turns[0], sessionRoot.SpanContext, loopID, "2")

	fake.admit(2)
	if err := rt.Send(ctx, event.DefaultTenant, loopID, "and Vail?"); err != nil {
		t.Fatalf("second Send: %v", err)
	}
	waitForKind(t, evCh, event.KindLoopParked, 3*time.Second)
	requireAlive(t, h, "after third park")
	flush(t, provider)
	turns = exportedNamed(t, exporter, "fuse.loop.turn")
	if len(turns) != 2 {
		t.Fatalf("exported fuse.loop.turn spans after two later turns = %d, want 2", len(turns))
	}
	assertTurnRoot(t, turns[1], sessionRoot.SpanContext, loopID, "3")

	// Still exactly one session root: a later turn must never restart fuse.loop.run.
	if n := len(exportedNamed(t, exporter, "fuse.loop.run")); n != 1 {
		t.Fatalf("fuse.loop.run spans = %d, want exactly 1 for the whole session", n)
	}
}

// assertTurnRoot checks the full per-turn contract: a NEW trace root (no parent, and
// a different trace id from the session root, since the session root is already
// exported), carrying loop_id / tenant / fuse.turn.index, ended, and causally LINKED
// to the session root so the per-turn traces remain attributable to one session.
func assertTurnRoot(t *testing.T, span tracetest.SpanStub, sessionRoot trace.SpanContext, loopID, wantIndex string) {
	t.Helper()
	if span.Parent.IsValid() {
		t.Errorf("turn span index %s is not a trace root: parent = %v", wantIndex, span.Parent)
	}
	if span.SpanContext.TraceID() == sessionRoot.TraceID() {
		t.Errorf("turn span index %s shares the session root's trace id; it must be its own trace", wantIndex)
	}
	if span.EndTime.IsZero() {
		t.Errorf("turn span index %s was exported without an end time", wantIndex)
	}
	if got := attr(span, "loop_id"); got != loopID {
		t.Errorf("turn span loop_id = %q, want %q", got, loopID)
	}
	if got := attr(span, "tenant"); got != string(event.DefaultTenant) {
		t.Errorf("turn span tenant = %q, want %q", got, event.DefaultTenant)
	}
	if got := attr(span, "fuse.turn.index"); got != wantIndex {
		t.Errorf("turn span fuse.turn.index = %q, want %q", got, wantIndex)
	}
	linked := false
	for _, l := range span.Links {
		if l.SpanContext.TraceID() == sessionRoot.TraceID() && l.SpanContext.SpanID() == sessionRoot.SpanID() {
			linked = true
		}
	}
	if !linked {
		t.Errorf("turn span index %s carries no link to the session root %v; links = %v", wantIndex, sessionRoot, span.Links)
	}
}

// requireAlive fails the test if the loop already finished — every assertion in this
// file is about what a LIVE, parked session has already exported.
func requireAlive(t *testing.T, h LoopHandle, when string) {
	t.Helper()
	select {
	case <-doneOf(h):
		t.Fatalf("interactive loop already finished %s; the criterion is about a still-alive session", when)
	default:
	}
}

func flush(t *testing.T, provider *sdktrace.TracerProvider) {
	t.Helper()
	if err := provider.ForceFlush(context.Background()); err != nil {
		t.Fatalf("ForceFlush: %v", err)
	}
}

// exportedNamed returns the exported spans with the given name, in export order.
func exportedNamed(t *testing.T, exporter *tracetest.InMemoryExporter, name string) []tracetest.SpanStub {
	t.Helper()
	var out []tracetest.SpanStub
	for _, s := range exporter.GetSpans() {
		if s.Name == name {
			out = append(out, s)
		}
	}
	return out
}

func attr(span tracetest.SpanStub, key string) string {
	for _, kv := range span.Attributes {
		if string(kv.Key) == key {
			return kv.Value.AsString()
		}
	}
	return ""
}
