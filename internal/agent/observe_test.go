package agent

import (
	"context"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/model"
	observeotel "github.com/ethanhinson/fuse/internal/observe/otel"
	"go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestToolTimeoutClosesSpanWithTimeoutOutcome(t *testing.T) {
	ex := tracetest.NewInMemoryExporter()
	tp := trace.NewTracerProvider(trace.WithSyncer(ex))
	a := New(&scriptedCompleter{}, &blockingExec{started: make(chan struct{}), canceled: make(chan struct{})}, nopRenderer{}, "m", "", 1, 1)
	a.SetObserver(observeotel.New(tp))
	a.SetToolTimeout(time.Millisecond)
	res := a.executeToolBounded(context.Background(), model.ToolCall{Name: "bash"})
	if !res.IsError {
		t.Fatal("timeout did not return a tool error")
	}
	spans := ex.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("spans=%d", len(spans))
	}
	got := ""
	for _, attr := range spans[0].Attributes {
		if string(attr.Key) == "fuse.outcome" {
			got = attr.Value.AsString()
		}
	}
	if got != "timeout" {
		t.Fatalf("outcome=%q", got)
	}
}
