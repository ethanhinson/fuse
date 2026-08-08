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

// TestGatewaySeamInjectsSummary drives a real agent turn through the production
// wiring — real model.Adapters POSTing to a scripted httptest gateway — and
// asserts Tier 2 works end-to-end at the gateway seam (learning
// verify-tool-loop-at-gateway-seam): the summarizer request (identified by its
// ToolChoice:"none" + ODSNF prompt) receives a canned summary, and the
// post-compaction MAIN request carries that injected summary text in place of
// the raw over-budget region — proving the loop wiring, not just a unit.
func TestGatewaySeamInjectsSummary(t *testing.T) {
	const cannedSummary = "Objective: verify seam\nState: compacted\nNext: proceed\nFiles: none"

	var (
		mu            sync.Mutex
		mainReqBody   string // the last non-summarizer request body seen
		summarizerHit bool
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		var sent struct {
			ToolChoice string `json:"tool_choice"`
			Messages   []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(b, &sent); err != nil {
			t.Errorf("unmarshal request: %v", err)
		}

		// The summarizer request is the one forcing text-only (ToolChoice "none")
		// carrying the ODSNF prompt scaffold.
		isSummarizer := sent.ToolChoice == "none" && strings.Contains(string(b), "Objective:")

		w.Header().Set("Content-Type", "application/json")
		if isSummarizer {
			mu.Lock()
			summarizerHit = true
			mu.Unlock()
			resp := map[string]any{
				"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": cannedSummary}}},
				"usage":   map[string]int{"prompt_tokens": 10, "completion_tokens": 5},
			}
			_ = json.NewEncoder(w).Encode(resp)
			return
		}

		// Main-loop request. Record its body, then end the turn without tool calls
		// so the loop terminates in one turn.
		mu.Lock()
		mainReqBody = string(b)
		mu.Unlock()
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"done"}}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`)
	}))
	defer srv.Close()

	// Real registry + real adapters wired the production way.
	reg := tools.NewRegistry()
	for _, tl := range tools.DefaultTools() {
		reg.Register(tl)
	}

	mainAdapter := model.NewAdapter(srv.URL, "tkn", srv.Client())
	a := agent.New(mainAdapter, reg, gwNopRenderer{}, "cloud/model", "", 2, 128)
	// Tiny context window so an over-budget turn is reachable within a short run.
	a.ContextWindow = 4000
	// Wire Tier 2 exactly as installSummarizer does: a bounded adapter labeled
	// "summarizer" against the same gateway; default no-op sink.
	sumAdapter := model.NewAdapter(srv.URL, "tkn", srv.Client())
	a.EnableSummarization(sumAdapter, "cloud/model", 2000, nil)

	// Over-budget history: three big old tool results push the estimate past the
	// 85% budget so the summarization pass fires before the first main request.
	big := strings.Repeat("x", 8000) // ~2000 tokens each
	history := []model.Message{
		{Role: "user", Content: "task"},
		{Role: "assistant", ToolCalls: []model.ToolCall{{ID: "1", Name: "read_file"}}},
		{Role: "tool", ToolCallID: "1", Name: "read_file", Content: big},
		{Role: "assistant", ToolCalls: []model.ToolCall{{ID: "2", Name: "read_file"}}},
		{Role: "tool", ToolCallID: "2", Name: "read_file", Content: big},
		{Role: "assistant", ToolCalls: []model.ToolCall{{ID: "3", Name: "grep"}}},
		{Role: "tool", ToolCallID: "3", Name: "grep", Content: big},
	}

	if _, err := a.Run(context.Background(), history); err != nil {
		t.Fatalf("agent run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if !summarizerHit {
		t.Fatal("summarizer request never reached the gateway — Tier 2 did not fire")
	}
	if mainReqBody == "" {
		t.Fatal("main request never reached the gateway")
	}
	if !strings.Contains(mainReqBody, "Objective: verify seam") {
		t.Errorf("post-compaction main request does not carry the injected summary; body:\n%s", mainReqBody)
	}
	// The raw over-budget region must have been stubbed (not sent verbatim).
	if strings.Count(mainReqBody, big) > 1 {
		t.Errorf("main request still carries the raw over-budget region verbatim")
	}
}
