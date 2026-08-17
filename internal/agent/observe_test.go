package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/model"
	observeotel "github.com/ethanhinson/fuse/internal/observe/otel"
	"go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// TestEveryModelCallIsObserved verifies the model-call choke points rather than
// synthesizing spans: primary and retry attempts run through Agent.Run, while
// the optional summarizer and relevance classifier use their production
// builders. A separately built child Agent mirrors the cmd child-builder seam.
func TestEveryModelCallIsObserved(t *testing.T) {
	ex := tracetest.NewInMemoryExporter()
	tp := trace.NewTracerProvider(trace.WithSyncer(ex))
	obs := observeotel.New(tp)
	var mu sync.Mutex
	requests := map[string]int{}
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode gateway request: %v", err)
		}
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		requests[req.Model]++
		call := requests[req.Model]
		mu.Unlock()
		if req.Model == "retry" && call == 1 {
			http.Error(w, "maximum context length exceeded", http.StatusBadRequest)
			return
		}
		content := "done"
		if req.Model == "summary" {
			content = "summary"
		}
		if req.Model == "classifier" {
			content = "0: 0.5"
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"`+content+`"}}]}`)
	}))
	defer gateway.Close()
	completer := model.NewAdapter(gateway.URL, "test-key", gateway.Client())
	completer.MaxAttempts = 1

	a := New(completer, &fakeExec{}, nopRenderer{}, "primary", "", 1, 1)
	a.SetObserver(obs)
	if _, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "run"}}); err != nil {
		t.Fatal(err)
	}

	// This is the same post-construction path production builders use when
	// installing the optional model-backed helpers.
	a.EnableSummarization(completer, "summary", 64, nil)
	if _, ok := a.summarizer.summarize(context.Background(), []model.Message{{Role: "tool", Content: "result"}}, ""); !ok {
		t.Fatal("summarizer did not complete")
	}
	a.EnableRelevanceClassifier(completer, "classifier", 1, 0, 1)
	classifier := a.relevanceScorer.(*classifierScorer)
	if _, ok := classifier.classify(context.Background(), []ToolResult{{ToolName: "read_file", Result: "x"}}, ScoreContext{Query: "q"}); !ok {
		t.Fatal("classifier did not complete")
	}

	// A child is built independently, just as each production child builder
	// constructs it before installing the shared observer.
	child := New(completer, &fakeExec{}, nopRenderer{}, "child", "", 1, 1)
	child.SetObserver(obs)
	if _, err := child.Run(context.Background(), []model.Message{{Role: "user", Content: "child task"}}); err != nil {
		t.Fatal(err)
	}

	// The context-recovery retry is an actual second primary gateway call.
	retryAgent := New(completer, &fakeExec{}, nopRenderer{}, "retry", "", 2, 1)
	retryAgent.SetObserver(obs)
	retryAgent.ContextWindow = 10_000
	retryHistory := []model.Message{{Role: "user", Content: "x"}}
	for range 4 {
		retryHistory = append(retryHistory, model.Message{Role: "tool", Content: strings.Repeat("x", 8_400)})
	}
	if _, err := retryAgent.Run(context.Background(), retryHistory); err != nil {
		t.Fatal(err)
	}

	if got := modelSpanCount(ex.GetSpans()); got != 6 {
		t.Fatalf("model observations = %d, want 6 (primary, retry attempt, retry, summarizer, classifier, child)", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if got := requests["primary"] + requests["summary"] + requests["classifier"] + requests["child"] + requests["retry"]; got != 6 {
		t.Fatalf("gateway calls = %d, want 6", got)
	}
}

func modelSpanCount(spans []tracetest.SpanStub) int {
	n := 0
	for _, span := range spans {
		if span.Name == "fuse.model.attempt.complete" {
			n++
		}
	}
	return n
}

func TestToolTimeoutClosesSpanWithTimeoutOutcome(t *testing.T) {
	ex := tracetest.NewInMemoryExporter()
	tp := trace.NewTracerProvider(trace.WithSyncer(ex))
	a := New(&scriptedCompleter{}, &blockingExec{started: make(chan struct{}), canceled: make(chan struct{})}, nopRenderer{}, "m", "", 1, 1)
	a.SetObserver(observeotel.New(tp))
	a.SetToolTimeout(time.Millisecond)
	res := a.executeToolBounded(context.Background(), 0, model.ToolCall{Name: "bash"})
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
