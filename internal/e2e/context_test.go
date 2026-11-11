package e2e

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

const prunedMarker = "old tool result cleared"

// paging builds a gateway that reads a large file a page per turn for turns turns,
// reporting realistic usage from the request size. Requests with no tools offered
// are the summarizer's, answered by summarize.
func paging(t *testing.T, turns int, summarize step, path *string) *server {
	t.Helper()
	main := 0
	return newRouter(t, func(r *request) step {
		if len(r.Tools) == 0 {
			return summarize
		}
		main++
		n := main
		usage := len(r.Raw) / 4
		return func(w http.ResponseWriter, _ *http.Request) {
			sseHeader(w)
			if n > turns {
				sse(w, contentChunk("finished paging"))
				sse(w, finishChunk("stop"))
			} else {
				sse(w, toolCallChunk(0, call{fmt.Sprintf("call_%d", n), "read_file",
					mustArgs(map[string]any{"path": *path, "start_line": n, "end_line": n + 200})}))
				sse(w, finishChunk("tool_calls"))
			}
			sse(w, usageChunk(usage, 5))
			done(w)
		}
	})
}

func bigFile(h *harness) string {
	var b strings.Builder
	for i := 0; i < 5000; i++ {
		fmt.Fprintf(&b, "%06d %s\n", i, strings.Repeat("z", 70))
	}
	p := h.path("big.txt")
	h.write(p, b.String(), 0o644)
	return p
}

func summaryCalls(srv *server) int {
	n := 0
	for _, r := range srv.requests() {
		if len(r.Tools) == 0 {
			n++
		}
	}
	return n
}

func TestCompaction_SummarizesAndFinishes(t *testing.T) {
	t.Parallel()
	var path string
	srv := paging(t, 12, text("SUMMARY-MARKER of earlier reads"), &path)
	h := newHarness(t, srv)
	h.mextra = "context_window: 24000"
	path = bigFile(h)
	r := h.run()
	wantCode(t, r, 0)
	wantContains(t, r.stdout, "finished paging", "stdout")
	if summaryCalls(srv) == 0 {
		t.Fatal("summarizer never called")
	}
	last := string(h.lastRequest().Raw)
	wantContains(t, last, "SUMMARY-MARKER", "final request")
	wantContains(t, last, prunedMarker, "final request")
	if est := len(last) / 4; est > 24000 {
		t.Fatalf("final request ~%d tokens exceeds the 24000 window", est)
	}
}

func TestCompaction_SummarizerFails_FallsBackToPruning(t *testing.T) {
	if testing.Short() {
		t.Skip("slow: summarizer retries back off")
	}
	t.Parallel()
	var path string
	srv := paging(t, 12, status(500, "summarizer down"), &path)
	h := newHarness(t, srv)
	h.mextra = "context_window: 24000"
	path = bigFile(h)
	r := h.run()
	wantCode(t, r, 0)
	wantContains(t, string(h.lastRequest().Raw), prunedMarker, "final request")
	// Every failure suppresses the next 5 turns, so 12 turns can't fail often.
	if c := summaryCalls(srv); c == 0 || c > 9 {
		t.Fatalf("%d summarizer attempts: want some, each bounded by suppression", c)
	}
}

// Disabled summarization goes straight to the deterministic prune.
func TestCompaction_SummarizationDisabled_StillPrunes(t *testing.T) {
	t.Parallel()
	var path string
	srv := paging(t, 12, status(500, "must not be called"), &path)
	h := newHarness(t, srv)
	h.mextra = "context_window: 24000"
	h.extra = "context:\n  summarization:\n    enabled: false\n"
	path = bigFile(h)
	wantCode(t, h.run(), 0)
	if c := summaryCalls(srv); c != 0 {
		t.Fatalf("%d summarizer calls with summarization off", c)
	}
	wantContains(t, string(h.lastRequest().Raw), prunedMarker, "final request")
}

// A provider rejecting for length after there's history: prune, retry once, stop.
func TestContextOverflow_AfterToolHistory_BoundedNoInfiniteLoop(t *testing.T) {
	t.Parallel()
	overflow := status(400, `{"error":{"message":"context_length_exceeded: input too long"}}`)
	var path string
	srv := newRouter(t, func(r *request) step {
		n := len(r.toolResults())
		switch {
		case n < 2:
			return tools(call{fmt.Sprintf("c%d", n), "read_file",
				mustArgs(map[string]any{"path": path, "start_line": n + 1, "end_line": n + 200})})
		case strings.Contains(string(r.Raw), prunedMarker):
			return text("recovered after prune")
		}
		return overflow
	})
	h := newHarness(t, srv)
	// Big enough that the two results fit the estimate, so the provider is the one
	// to refuse, and small enough that the recovery prune can clear the older one.
	h.mextra = "context_window: 20000"
	path = bigFile(h)
	r := h.run()
	wantCode(t, r, 0)
	wantContains(t, r.stdout, "recovered after prune", "stdout")
	if srv.count() != 4 {
		t.Fatalf("requests = %d, want 2 reads, 1 refusal, 1 retry", srv.count())
	}
}

// No context_window is set, so the first rejection teaches the loop a window.
// The read and the rejected request are sent; the only tool result is the
// newest, which pruning always keeps, so the run aborts against the learned
// window without another request. Even with something prunable, a second
// rejection can only learn a smaller window, so the count stays bounded; a
// higher count means the learned window is not shrinking and the loop could spin.
func TestContextOverflow_Persistent_Stops(t *testing.T) {
	t.Parallel()
	overflow := status(400, `{"error":{"message":"maximum context length exceeded"}}`)
	var path string
	srv := newRouter(t, func(r *request) step {
		if !hasToolResult(r) {
			return tool("read_file", map[string]any{"path": path, "start_line": 1, "end_line": 400})
		}
		return overflow
	})
	h := newHarness(t, srv)
	path = bigFile(h)
	r := h.run()
	wantCode(t, r, 1)
	if srv.count() > 4 {
		t.Fatalf("requests = %d, want prune-and-retry at most once", srv.count())
	}
	wantContains(t, r.stdout+r.stderr, "learned window", "output")
}

// A context genuinely too small to hold the conversation ends the run with a clear
// error rather than spinning.
func TestContextTooLarge_Aborts(t *testing.T) {
	t.Parallel()
	srv := newRouter(t, func(r *request) step {
		return func(w http.ResponseWriter, _ *http.Request) {
			sseHeader(w)
			sse(w, toolCallChunk(0, call{"c", "list_directory", mustArgs(map[string]any{"path": ".", "n": r.N})}))
			sse(w, finishChunk("tool_calls"))
			sse(w, usageChunk(5000, 5))
			done(w)
		}
	})
	h := newHarness(t, srv)
	h.mextra = "context_window: 1000"
	r := h.run()
	wantCode(t, r, 1)
	wantContains(t, r.stderr, "context too large", "stderr")
	if srv.count() > 3 {
		t.Fatalf("requests = %d", srv.count())
	}
}
