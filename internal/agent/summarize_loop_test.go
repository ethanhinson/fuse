package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/tools"
)

// countingSummarizerCompleter counts summarizer calls and returns a scripted
// summary (or error) for each. It is distinct from the main-loop completer so a
// test can assert the summarizer was (or was not) called.
type countingSummarizerCompleter struct {
	calls   int
	summary string
	err     error
	// perCall, when non-nil, overrides summary/err per call index.
	perCall func(n int) (string, error)
}

func (c *countingSummarizerCompleter) Complete(ctx context.Context, req model.CompletionReq) (model.CompletionResp, error) {
	c.calls++
	if c.perCall != nil {
		s, err := c.perCall(c.calls)
		if err != nil {
			return model.CompletionResp{}, err
		}
		return model.CompletionResp{Content: s}, nil
	}
	if c.err != nil {
		return model.CompletionResp{}, c.err
	}
	return model.CompletionResp{Content: c.summary}, nil
}

// overBudgetHistory builds a history whose old tool results push the estimate
// over budget for a 4000-token window (budget 3400, protection 1000).
func overBudgetHistory() []model.Message {
	big := strings.Repeat("x", 8000) // ~2000 tokens each
	return []model.Message{
		{Role: "user", Content: "task"},
		{Role: "assistant", ToolCalls: []model.ToolCall{{ID: "1", Name: "read_file"}}},
		{Role: "tool", ToolCallID: "1", Name: "read_file", Content: big},
		{Role: "assistant", ToolCalls: []model.ToolCall{{ID: "2", Name: "read_file"}}},
		{Role: "tool", ToolCallID: "2", Name: "read_file", Content: big},
		{Role: "assistant", ToolCalls: []model.ToolCall{{ID: "3", Name: "grep"}}},
		{Role: "tool", ToolCallID: "3", Name: "grep", Content: big},
	}
}

func TestRunSummarizesBeforePruning(t *testing.T) {
	comp := &scriptedCompleter{responses: []model.CompletionResp{{Content: "done"}}}
	a := New(comp, &fakeExec{}, nopRenderer{}, "m", "", 10, 100)
	a.ContextWindow = 4000

	sc := &countingSummarizerCompleter{summary: "Objective: read files\nNext: continue"}
	a.SetSummarizer(newSummarizer(sc, "m", 2000), nil)

	hist, err := a.Run(context.Background(), overBudgetHistory())
	if err != nil {
		t.Fatalf("run err = %v", err)
	}
	if sc.calls != 1 {
		t.Fatalf("summarizer calls = %d, want 1 (summarize before prune)", sc.calls)
	}
	if comp.i == 0 {
		t.Fatal("main completer never called — turn should proceed after compaction")
	}
	// A summary message must be present in the returned history.
	found := false
	for _, m := range hist {
		if strings.Contains(m.Content, "Objective: read files") {
			found = true
		}
	}
	if !found {
		t.Errorf("injected summary not found in history")
	}
	// Pairing must remain valid after injection + prune.
	if err := checkPairing(hist); err != nil {
		t.Errorf("pairing invalid after summarization: %v", err)
	}
}

// TestRunSummarizerFailFallsBackToTier1: with the summarizer forced to fail, the
// loop's post-state matches the pure Tier-1 prune path byte-for-byte.
func TestRunSummarizerFailFallsBackToTier1(t *testing.T) {
	// Tier-1 baseline: no summarizer wired.
	base := New(&scriptedCompleter{responses: []model.CompletionResp{{Content: "done"}}}, &fakeExec{}, nopRenderer{}, "m", "", 10, 100)
	base.ContextWindow = 4000
	baseHist, err := base.Run(context.Background(), overBudgetHistory())
	if err != nil {
		t.Fatalf("baseline run err = %v", err)
	}

	// Summarizer wired but failing.
	comp := &scriptedCompleter{responses: []model.CompletionResp{{Content: "done"}}}
	a := New(comp, &fakeExec{}, nopRenderer{}, "m", "", 10, 100)
	a.ContextWindow = 4000
	sc := &countingSummarizerCompleter{err: errors.New("timeout awaiting response headers")}
	a.SetSummarizer(newSummarizer(sc, "m", 2000), nil)

	failHist, err := a.Run(context.Background(), overBudgetHistory())
	if err != nil {
		t.Fatalf("fail-path run err = %v", err)
	}
	if sc.calls != 1 {
		t.Fatalf("summarizer calls = %d, want 1 (attempted once)", sc.calls)
	}
	if len(failHist) != len(baseHist) {
		t.Fatalf("fail path len %d != baseline len %d — not identical to Tier-1", len(failHist), len(baseHist))
	}
	for i := range baseHist {
		if failHist[i].Content != baseHist[i].Content || failHist[i].Role != baseHist[i].Role {
			t.Errorf("msg %d differs from Tier-1 baseline: %q/%q vs %q/%q",
				i, failHist[i].Role, failHist[i].Content, baseHist[i].Role, baseHist[i].Content)
		}
	}
}

// TestRunSuppressionAfterFailure: after a failure, the next N over-budget turns
// do not call the summarizer, then it resumes.
func TestRunSuppressionAfterFailure(t *testing.T) {
	// The main loop keeps requesting the same tool so every turn re-enters the
	// over-budget branch; the tool result is huge so budget stays exceeded.
	huge := strings.Repeat("z", 8000)
	mainComp := &scriptedCompleter{}
	// Script: always ask for a tool call, so the loop keeps turning and stays over budget.
	for i := 0; i < summarizeSuppressTurns+3; i++ {
		mainComp.responses = append(mainComp.responses,
			model.CompletionResp{ToolCalls: []model.ToolCall{{ID: "t", Name: "read_file", Arguments: "{}"}}})
	}
	exec := &hugeExec{output: huge}
	a := New(mainComp, exec, nopRenderer{}, "m", "", summarizeSuppressTurns+2, 100)
	a.ContextWindow = 4000

	// Summarizer fails on the FIRST call, would succeed after — but suppression
	// must prevent any call during the window.
	sc := &countingSummarizerCompleter{perCall: func(n int) (string, error) {
		if n == 1 {
			return "", errors.New("boom")
		}
		return "Objective: ok", nil
	}}
	a.SetSummarizer(newSummarizer(sc, "m", 2000), nil)

	_, _ = a.Run(context.Background(), overBudgetHistory())

	// The summarizer should have been called exactly once (the failing call);
	// subsequent over-budget turns in the suppression window must not call it.
	if sc.calls != 1 {
		t.Errorf("summarizer calls = %d, want 1 (suppressed after first failure)", sc.calls)
	}
}

// hugeExec returns a large fixed output so the loop stays over budget.
type hugeExec struct{ output string }

func (h *hugeExec) Schemas() []model.ToolSchema { return nil }
func (h *hugeExec) Execute(ctx context.Context, name, args string) tools.Result {
	return tools.Result{Output: h.output}
}

// TestRunAnchoringAcrossTwoCompactions: two successive compactions pass v1 into
// the second call; only one summary lives in context at a time. Each turn emits
// TWO large tool results so a fresh unprotected result accumulates every turn,
// re-crossing budget and re-triggering compaction deterministically.
func TestRunAnchoringAcrossTwoCompactions(t *testing.T) {
	big := strings.Repeat("q", 8000) // ~2000 tokens each; 2/turn = ~4000 > budget
	mainComp := &scriptedCompleter{}
	for i := 0; i < 6; i++ {
		mainComp.responses = append(mainComp.responses,
			model.CompletionResp{ToolCalls: []model.ToolCall{
				{ID: "a", Name: "read_file", Arguments: "{}"},
				{ID: "b", Name: "read_file", Arguments: "{}"},
			}})
	}
	exec := &hugeExec{output: big}
	a := New(mainComp, exec, nopRenderer{}, "m", "", 6, 100)
	a.ContextWindow = 4000 // budget 3400, protect 1000

	var prevSeen []string
	sc := &countingSummarizerCompleter{perCall: func(n int) (string, error) {
		return "SUMMARY_V" + string(rune('0'+n)), nil
	}}
	captor := &prevCaptor{inner: sc, seen: &prevSeen}
	a.SetSummarizer(newSummarizer(captor, "m", 2000), nil)

	hist, _ := a.Run(context.Background(), overBudgetHistory())

	if sc.calls < 2 {
		t.Fatalf("only %d summarizer call(s) — expected >= 2 compactions", sc.calls)
	}
	// The second summarizer call must have carried the first summary (anchoring).
	if len(prevSeen) < 2 || !strings.Contains(prevSeen[1], "SUMMARY_V1") {
		t.Errorf("second compaction did not carry previous summary; seen=%v", prevSeen)
	}
	// At most one summary message lives in the final history (anchored, not
	// stacked): a prior summary is an assistant message the region selector never
	// re-summarizes, and each compaction replaces the living document.
	count := 0
	for _, m := range hist {
		if strings.Contains(m.Content, summaryHeader) {
			count++
		}
	}
	if count > 1 {
		t.Errorf("found %d summary messages in context, want at most 1 (anchored)", count)
	}
}

// prevCaptor records the previous-summary content the summarizer was asked to
// fold in on each call (extracted from the PREVIOUS SUMMARY user message).
type prevCaptor struct {
	inner *countingSummarizerCompleter
	seen  *[]string
}

func (p *prevCaptor) Complete(ctx context.Context, req model.CompletionReq) (model.CompletionResp, error) {
	prev := ""
	for _, m := range req.Messages {
		if strings.HasPrefix(m.Content, "PREVIOUS SUMMARY:") {
			prev = m.Content
		}
	}
	*p.seen = append(*p.seen, prev)
	return p.inner.Complete(ctx, req)
}

func TestRunDisabledIsPureTier1(t *testing.T) {
	// No summarizer wired ⇒ identical to Tier-1 (guarded by the fail-fallback
	// test too, but assert the summarizer is simply never constructed here).
	comp := &scriptedCompleter{responses: []model.CompletionResp{{Content: "done"}}}
	a := New(comp, &fakeExec{}, nopRenderer{}, "m", "", 10, 100)
	a.ContextWindow = 4000
	hist, err := a.Run(context.Background(), overBudgetHistory())
	if err != nil {
		t.Fatalf("run err = %v", err)
	}
	// Old tool results are stubbed, nothing summarized.
	for _, m := range hist {
		if strings.Contains(m.Content, summaryHeader) {
			t.Errorf("summary injected with no summarizer wired")
		}
	}
}

// TestRunSegmentSinkRecordsAndPointerAppears: a fake sink returning a path makes
// the recovery pointer line appear; the region metadata is recorded.
func TestRunSegmentSinkRecordsAndPointerAppears(t *testing.T) {
	comp := &scriptedCompleter{responses: []model.CompletionResp{{Content: "done"}}}
	a := New(comp, &fakeExec{}, nopRenderer{}, "m", "", 10, 100)
	a.ContextWindow = 4000
	sc := &countingSummarizerCompleter{summary: "Objective: x"}
	sink := &fakeSink{pointer: "/seg/0001"}
	a.SetSummarizer(newSummarizer(sc, "m", 2000), sink)

	hist, err := a.Run(context.Background(), overBudgetHistory())
	if err != nil {
		t.Fatalf("run err = %v", err)
	}
	if sink.calls != 1 {
		t.Fatalf("sink.Archive calls = %d, want 1", sink.calls)
	}
	if len(sink.last.Messages) == 0 {
		t.Errorf("sink received an empty region")
	}
	if sink.last.TokensBefore <= sink.last.TokensAfter {
		t.Errorf("sink savings metadata wrong: before=%d after=%d", sink.last.TokensBefore, sink.last.TokensAfter)
	}
	if len(sink.last.ToolNames) == 0 {
		t.Errorf("sink received no tool names")
	}
	foundPtr := false
	for _, m := range hist {
		if strings.Contains(m.Content, "grep your past at /seg/0001") {
			foundPtr = true
		}
	}
	if !foundPtr {
		t.Errorf("recovery pointer line missing despite non-empty sink pointer")
	}
}

// TestRunSegmentSinkErrorNotFatal: a sink returning an error is logged, not
// fatal; the turn still proceeds with the summary (no pointer).
func TestRunSegmentSinkErrorNotFatal(t *testing.T) {
	comp := &scriptedCompleter{responses: []model.CompletionResp{{Content: "done"}}}
	a := New(comp, &fakeExec{}, nopRenderer{}, "m", "", 10, 100)
	a.ContextWindow = 4000
	sc := &countingSummarizerCompleter{summary: "Objective: x"}
	sink := &fakeSink{err: errors.New("disk full")}
	a.SetSummarizer(newSummarizer(sc, "m", 2000), sink)

	hist, err := a.Run(context.Background(), overBudgetHistory())
	if err != nil {
		t.Fatalf("sink error must not fail the turn; got %v", err)
	}
	for _, m := range hist {
		if strings.Contains(m.Content, "grep your past") {
			t.Errorf("pointer line present despite sink error")
		}
	}
}
