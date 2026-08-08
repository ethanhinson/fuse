package agent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/model"
)

// captureCompleter records the last request and returns a scripted response.
type captureCompleter struct {
	lastReq model.CompletionReq
	calls   int
	resp    model.CompletionResp
	err     error
}

func (c *captureCompleter) Complete(ctx context.Context, req model.CompletionReq) (model.CompletionResp, error) {
	c.calls++
	c.lastReq = req
	if c.err != nil {
		return model.CompletionResp{}, c.err
	}
	return c.resp, nil
}

func toolMsg(name, content string) model.Message {
	return model.Message{Role: "tool", Name: name, ToolCallID: "id-" + name, Content: content}
}

func TestSummarizeBuildsODSNFTextOnlyRequest(t *testing.T) {
	cc := &captureCompleter{resp: model.CompletionResp{Content: "Objective: x\nDetails: y"}}
	s := newSummarizer(cc, "cheap/model", 2000)
	region := []model.Message{toolMsg("read_file", "file body here")}

	out, ok := s.summarize(context.Background(), region, "")
	if !ok {
		t.Fatal("summarize returned ok=false, want true")
	}
	if out != "Objective: x\nDetails: y" {
		t.Errorf("summary = %q, want the model text", out)
	}
	if cc.lastReq.ToolChoice != "none" {
		t.Errorf("ToolChoice = %q, want none (text-only)", cc.lastReq.ToolChoice)
	}
	if cc.lastReq.MaxTokens != 2000 {
		t.Errorf("MaxTokens = %d, want 2000 (max_output)", cc.lastReq.MaxTokens)
	}
	if cc.lastReq.Model != "cheap/model" {
		t.Errorf("Model = %q, want cheap/model", cc.lastReq.Model)
	}
	if len(cc.lastReq.Tools) != 0 {
		t.Errorf("Tools = %v, want none for a summarizer call", cc.lastReq.Tools)
	}
	// The prompt must carry the ODSNF instruction and the region content.
	joined := reqText(cc.lastReq)
	for _, want := range []string{"Objective", "Details", "State", "Next", "Files", "file body here"} {
		if !strings.Contains(joined, want) {
			t.Errorf("request missing %q; got:\n%s", want, joined)
		}
	}
}

func TestSummarizeAnchoringIncludesPreviousSummary(t *testing.T) {
	cc := &captureCompleter{resp: model.CompletionResp{Content: "summary_v2"}}
	s := newSummarizer(cc, "m", 2000)
	region := []model.Message{toolMsg("grep", "matches")}

	out, ok := s.summarize(context.Background(), region, "summary_v1 PREVIOUS")
	if !ok {
		t.Fatal("ok=false, want true")
	}
	if out != "summary_v2" {
		t.Errorf("summary = %q, want summary_v2", out)
	}
	joined := reqText(cc.lastReq)
	if !strings.Contains(joined, "summary_v1 PREVIOUS") {
		t.Errorf("previous summary not carried into request:\n%s", joined)
	}
	if !strings.Contains(strings.ToLower(joined), "update") {
		t.Errorf("expected an update-in-place instruction for anchoring:\n%s", joined)
	}
}

func TestSummarizeFailsSafeOnError(t *testing.T) {
	cc := &captureCompleter{err: errors.New("timeout awaiting response headers")}
	s := newSummarizer(cc, "m", 2000)
	out, ok := s.summarize(context.Background(), []model.Message{toolMsg("read_file", "x")}, "")
	if ok || out != "" {
		t.Errorf("on transport error got (%q, %v), want (\"\", false)", out, ok)
	}
}

func TestSummarizeFailsSafeOnEmptyOutput(t *testing.T) {
	cc := &captureCompleter{resp: model.CompletionResp{Content: "   "}}
	s := newSummarizer(cc, "m", 2000)
	out, ok := s.summarize(context.Background(), []model.Message{toolMsg("read_file", "x")}, "")
	if ok || out != "" {
		t.Errorf("on empty output got (%q, %v), want (\"\", false)", out, ok)
	}
}

// TestSummarizerBoundedCallLabeledTrace drives the real bounded adapter against
// an httptest gateway and asserts a [summarizer]-labeled REQ/RESP block appears
// in the trace (learning bound-every-model-call).
func TestSummarizerBoundedCallLabeledTrace(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Objective: done"}}],"usage":{"prompt_tokens":5,"completion_tokens":3}}`))
	}))
	defer srv.Close()

	var trace strings.Builder
	adapter := model.NewAdapter(srv.URL, "tkn", srv.Client()).WithTraceLabel(&trace, "summarizer")
	s := newSummarizer(adapter, "cloud/model", 2000)

	out, ok := s.summarize(context.Background(), []model.Message{toolMsg("read_file", "body")}, "")
	if !ok || out == "" {
		t.Fatalf("summarize = (%q, %v), want a non-empty summary", out, ok)
	}
	tr := trace.String()
	if !strings.Contains(tr, "── REQ [summarizer] ──") || !strings.Contains(tr, "── RESP [summarizer] ──") {
		t.Errorf("trace missing [summarizer]-labeled REQ/RESP block:\n%s", tr)
	}
}

// TestSummarizeLadderDropsOldestTurns: a region exceeding the summarizer input
// budget shrinks by dropping oldest turns and still returns a summary; the rung
// taken is reported.
func TestSummarizeLadderDropsOldestTurns(t *testing.T) {
	cc := &captureCompleter{resp: model.CompletionResp{Content: "summary"}}
	s := newSummarizer(cc, "m", 2000)
	// Budget above the fixed prompt scaffold (~180 tok) but below a full big
	// tool result, so the oldest turns must be dropped to fit.
	s.inputBudgetTokens = 300

	big := strings.Repeat("x", 2000) // ~500 tokens each
	region := []model.Message{
		toolMsg("read_file", big),
		toolMsg("read_file", big),
		toolMsg("grep", "small tail"),
	}
	out, ok, rung := s.summarizeWithRung(context.Background(), region, "")
	if !ok || out == "" {
		t.Fatalf("summarize = (%q, %v), want a summary after ladder", out, ok)
	}
	if rung != rungDropOldest {
		t.Errorf("rung = %v, want rungDropOldest", rung)
	}
	// The surviving input must be smaller than the full region.
	if messagesSize(cc.lastReq.Messages)/bytesPerToken > s.inputBudgetTokens {
		t.Errorf("request still exceeds input budget after ladder")
	}
}

// TestSummarizeLadderStripsToolOutputs: when dropping turns is not enough, tool
// outputs are stripped from the surviving region.
func TestSummarizeLadderStripsToolOutputs(t *testing.T) {
	cc := &captureCompleter{resp: model.CompletionResp{Content: "summary"}}
	s := newSummarizer(cc, "m", 2000)
	// Budget above the scaffold + stubs but below a single full tool result:
	// dropping oldest cannot fit (the last message alone overflows), so tool
	// outputs are stripped to the stub.
	s.inputBudgetTokens = 250

	big := strings.Repeat("y", 4000)
	region := []model.Message{
		toolMsg("read_file", big),
		toolMsg("grep", big),
	}
	out, ok, rung := s.summarizeWithRung(context.Background(), region, "")
	if !ok || out == "" {
		t.Fatalf("summarize = (%q, %v), want a summary after strip rung", out, ok)
	}
	if rung != rungStripOutputs {
		t.Errorf("rung = %v, want rungStripOutputs", rung)
	}
}

// TestSummarizeLadderExhaustionFallsBack: if even the smallest rung overflows,
// summarize returns the no-summary signal (caller falls back to Tier-1).
func TestSummarizeLadderExhaustionFallsBack(t *testing.T) {
	cc := &captureCompleter{resp: model.CompletionResp{Content: "summary"}}
	s := newSummarizer(cc, "m", 2000)
	s.inputBudgetTokens = 0 // nothing fits, even the prompt scaffold

	region := []model.Message{toolMsg("read_file", strings.Repeat("z", 100))}
	out, ok := s.summarize(context.Background(), region, "")
	if ok || out != "" {
		t.Errorf("ladder-exhausted got (%q, %v), want (\"\", false)", out, ok)
	}
	if cc.calls != 0 {
		t.Errorf("summarizer made %d model calls on ladder exhaustion, want 0", cc.calls)
	}
}

// reqText concatenates the content of every message in a request for assertion.
func reqText(req model.CompletionReq) string {
	var b strings.Builder
	for _, m := range req.Messages {
		b.WriteString(m.Role)
		b.WriteString(": ")
		b.WriteString(m.Content)
		b.WriteString("\n")
	}
	return b.String()
}

// --- Task 4: summary message assembly + recovery-pointer rule ---

func TestSummaryMessageOmitsPointerWhenEmpty(t *testing.T) {
	m := buildSummaryMessage("Objective: x\nNext: y", "")
	if strings.Contains(m.Content, "grep your past") {
		t.Errorf("empty pointer must omit the recovery line; got:\n%s", m.Content)
	}
	if !strings.Contains(m.Content, "Objective: x") {
		t.Errorf("summary body missing:\n%s", m.Content)
	}
}

func TestSummaryMessageIncludesPointerWhenPresent(t *testing.T) {
	m := buildSummaryMessage("Objective: x", "/home/u/.fuse/sessions/s1/segments/0003")
	if !strings.Contains(m.Content, "grep your past") {
		t.Errorf("non-empty pointer must include the recovery line; got:\n%s", m.Content)
	}
	if !strings.Contains(m.Content, "/home/u/.fuse/sessions/s1/segments/0003") {
		t.Errorf("recovery line must carry the pointer path; got:\n%s", m.Content)
	}
}

func TestSummaryMessagePairingValid(t *testing.T) {
	// The injected summary must not be a tool-role message (which pruneOldToolResults
	// could re-stub) and must not carry an orphaned tool_call. An assistant text
	// message with no ToolCalls is the safe shape.
	m := buildSummaryMessage("Objective: x", "")
	if m.Role == "tool" {
		t.Errorf("summary injected as a tool message would be re-stubbed by pruning")
	}
	if len(m.ToolCalls) != 0 {
		t.Errorf("summary message carries %d tool_calls; would orphan a pair", len(m.ToolCalls))
	}

	// Splice the summary in place of a raw tool-result span and assert pairing
	// stays valid: every tool-role message references an assistant tool_call that
	// precedes it, and every assistant tool_call has a matching tool result.
	history := []model.Message{
		{Role: "user", Content: "do it"},
		{Role: "assistant", ToolCalls: []model.ToolCall{{ID: "c1", Name: "read_file"}}},
		toolMsg2("c1", "read_file", "old body"), // this span gets replaced
		m,                                       // injected summary at the boundary
		{Role: "assistant", ToolCalls: []model.ToolCall{{ID: "c2", Name: "grep"}}},
		toolMsg2("c2", "grep", "recent"),
	}
	// Wait: replacing c1's result with the summary would orphan c1. In the loop
	// the summary REPLACES the raw span including the assistant tool_call that
	// produced it. Model the post-replacement history: the c1 pair is gone,
	// summary sits where it was.
	post := []model.Message{
		{Role: "user", Content: "do it"},
		m,
		{Role: "assistant", ToolCalls: []model.ToolCall{{ID: "c2", Name: "grep"}}},
		toolMsg2("c2", "grep", "recent"),
	}
	if err := checkPairing(post); err != nil {
		t.Errorf("post-injection pairing invalid: %v", err)
	}
	// Pre-replacement history should also validate (sanity that checkPairing works).
	if err := checkPairing(history); err != nil {
		t.Errorf("pre-injection sanity pairing invalid: %v", err)
	}
	// A prune pass over the post history must not touch the summary.
	before := post[1].Content
	pruneOldToolResults(post, 0)
	if post[1].Content != before {
		t.Errorf("prune re-stubbed the summary message: %q -> %q", before, post[1].Content)
	}
}

func toolMsg2(callID, name, content string) model.Message {
	return model.Message{Role: "tool", ToolCallID: callID, Name: name, Content: content}
}

// checkPairing asserts every tool-role message references a preceding assistant
// tool_call id and every assistant tool_call has a following tool result — the
// provider pairing invariant the injection must preserve.
func checkPairing(msgs []model.Message) error {
	open := map[string]bool{}
	for _, m := range msgs {
		switch m.Role {
		case "assistant":
			for _, tc := range m.ToolCalls {
				open[tc.ID] = true
			}
		case "tool":
			if m.ToolCallID == "" || !open[m.ToolCallID] {
				return errPairing("tool result with no matching open tool_call: id=" + m.ToolCallID)
			}
			delete(open, m.ToolCallID)
		}
	}
	if len(open) > 0 {
		return errPairing("unanswered tool_call remains open")
	}
	return nil
}

type pairingErr string

func (e pairingErr) Error() string { return string(e) }
func errPairing(s string) error    { return pairingErr(s) }
