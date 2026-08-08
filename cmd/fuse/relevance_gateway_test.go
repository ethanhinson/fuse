package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/tools"
)

// relevanceClassifierPromptMarker is a stable substring of the classifier
// prompt used to identify its request at the gateway seam.
const relevanceClassifierPromptMarker = "relevance score in [0,1]"

// buildOverBudgetHistory returns a history whose old tool results push the
// estimate past budget, mixing tool types so at least one lands in the
// classifier's borderline band.
// relevanceTestWindow is the context window for the gateway tests: sized so the
// history below is over budget, yet pruning brings it under and the recency
// floor still leaves a positive remainder for phase-2 relevance scoring (where
// the classifier fires).
const relevanceTestWindow = 8000

func buildOverBudgetHistory() []model.Message {
	// Many small results (~300 tokens each) so the total blows the ~6.8k budget
	// while the ~1k recency floor protects only the few newest, leaving budget
	// for phase-2 scoring of the rest — that scoring is what invokes the
	// classifier for borderline candidates.
	blob := strings.Repeat("x", 1200) // ~300 tokens each
	msgs := []model.Message{
		{Role: "user", Content: "unrelated task"},
	}
	for i := 1; i <= 30; i++ {
		name := "grep"
		if i%2 == 0 {
			name = "bash"
		}
		id := indexDigits(i)
		msgs = append(msgs,
			model.Message{Role: "assistant", ToolCalls: []model.ToolCall{{ID: id, Name: name, Arguments: "q"}}},
			model.Message{Role: "tool", ToolCallID: id, Name: name, Content: blob},
		)
	}
	return msgs
}

// indexDigits renders i as its base-10 digit string.
func indexDigits(i int) string {
	if i == 0 {
		return "0"
	}
	digits := "0123456789"
	var s string
	for n := i; n > 0; n /= 10 {
		s = string(digits[n%10]) + s
	}
	return s
}

// TestGatewaySeamRelevanceClassifierFires drives a real over-budget agent turn
// through production wiring (real model.Adapters → scripted httptest gateway)
// with a classifier model configured, and asserts the classifier request
// reaches the gateway seam (learning verify-tool-loop-at-gateway-seam): a
// borderline candidate is batched to the classifier before the main request.
func TestGatewaySeamRelevanceClassifierFires(t *testing.T) {
	var (
		mu             sync.Mutex
		classifierHit  bool
		classifierBody string
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body := string(b)
		w.Header().Set("Content-Type", "application/json")

		if strings.Contains(body, relevanceClassifierPromptMarker) {
			mu.Lock()
			classifierHit = true
			classifierBody = body
			mu.Unlock()
			// Return well-formed scores for a generous number of indices so any
			// batch size is covered.
			var sb strings.Builder
			for i := 0; i < 20; i++ {
				sb.WriteString(itoaLine(i))
			}
			resp := map[string]any{
				"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": sb.String()}}},
				"usage":   map[string]int{"prompt_tokens": 10, "completion_tokens": 5},
			}
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"done"}}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`)
	}))
	defer srv.Close()

	a := newRelevanceTestAgent(t, srv)
	if _, err := a.Run(context.Background(), buildOverBudgetHistory()); err != nil {
		t.Fatalf("agent run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if !classifierHit {
		t.Fatal("classifier request never reached the gateway — the borderline band was not classified")
	}
	// Only borderline candidates are sent — the batch must not contain every
	// candidate necessarily, but must be a valid indexed candidate block.
	if !strings.Contains(classifierBody, "0: tool=") {
		t.Errorf("classifier request missing indexed candidate block; body:\n%s", classifierBody)
	}
}

// TestGatewaySeamRelevanceClassifierFailsSafe asserts a classifier that errors
// at the gateway falls the run back to the heuristic ranking with no stubbing
// regression: the run still completes and the newest result survives.
func TestGatewaySeamRelevanceClassifierFailsSafe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body := string(b)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(body, relevanceClassifierPromptMarker) {
			// Classifier fails: 500. The scorer must fall back to the heuristic.
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, `{"error":"boom"}`)
			return
		}
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"done"}}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`)
	}))
	defer srv.Close()

	a := newRelevanceTestAgent(t, srv)
	hist, err := a.Run(context.Background(), buildOverBudgetHistory())
	if err != nil {
		t.Fatalf("classifier failure must not fail the run (fail-safe): %v", err)
	}
	// The newest tool result must never be stubbed — the recency floor plus the
	// heuristic fallback protect it regardless of classifier state.
	const stub = "[old tool result cleared to free context — re-run the tool if needed]"
	newest := -1
	for i, m := range hist {
		if m.Role == "tool" {
			newest = i
		}
	}
	if newest < 0 {
		t.Fatal("no tool result in history")
	}
	if hist[newest].Content == stub {
		t.Error("newest tool result stubbed despite recency floor — fail-safe regression")
	}
}

// newRelevanceTestAgent builds an agent wired the production way with a
// classifier model configured against srv.
func newRelevanceTestAgent(t *testing.T, srv *httptest.Server) *agent.Agent {
	t.Helper()
	reg := tools.NewRegistry()
	for _, tl := range tools.DefaultTools() {
		reg.Register(tl)
	}
	mainAdapter := model.NewAdapter(srv.URL, "tkn", srv.Client())
	a := agent.New(mainAdapter, reg, gwNopRenderer{}, "cloud/model", "", 2, 128)
	a.ContextWindow = relevanceTestWindow
	// Wire the classifier exactly as installRelevance does: a bounded adapter
	// against the same gateway, spec-default band + batch.
	clsAdapter := model.NewAdapter(srv.URL, "tkn", srv.Client())
	a.EnableRelevanceClassifier(clsAdapter, "cloud/model", 10, 0.30, 0.60)
	return a
}

// itoaLine formats one "N: 0.45" classifier score line (a mid score so parsing
// is exercised without steering selection).
func itoaLine(i int) string {
	digits := "0123456789"
	var idx string
	if i == 0 {
		idx = "0"
	} else {
		for n := i; n > 0; n /= 10 {
			idx = string(digits[n%10]) + idx
		}
	}
	return idx + ": 0.45\n"
}
