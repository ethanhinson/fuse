package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/tools"
)

// capCompleter replays responses and records the cap each request asked for.
type capCompleter struct {
	responses []model.CompletionResp
	caps      []int
	lastMsgs  []model.Message
	err       error
	errAtCap  int
}

func (c *capCompleter) Complete(_ context.Context, req model.CompletionReq) (model.CompletionResp, error) {
	c.caps = append(c.caps, req.MaxTokens)
	c.lastMsgs = req.Messages
	if c.errAtCap > 0 && req.MaxTokens >= c.errAtCap {
		return model.CompletionResp{}, c.err
	}
	i := len(c.caps) - 1
	if i >= len(c.responses) {
		return c.responses[len(c.responses)-1], nil
	}
	return c.responses[i], nil
}

func cutOff(content string, calls ...model.ToolCall) model.CompletionResp {
	return model.CompletionResp{Content: content, ToolCalls: calls, FinishReason: "length", InputTokens: 100, OutputTokens: 1000}
}

func runCapped(t *testing.T, comp *capCompleter, exec ToolExecutor, maxTokens int) ([]model.Message, *recordingStore, error) {
	t.Helper()
	a := New(comp, exec, nopRenderer{}, "m", "", 10, maxTokens)
	rec := &recordingStore{}
	a.SetEventSink(rec)
	hist, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "task"}})
	return hist, rec, err
}

func decodeCallEnds(t *testing.T, rec *recordingStore) []event.ModelCallEndPayload {
	t.Helper()
	var out []event.ModelCallEndPayload
	for _, e := range rec.events {
		if e.Kind != event.KindModelCallEnd {
			continue
		}
		var p event.ModelCallEndPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

// The incident: every token went to reasoning, nothing came back, and the run
// ended as a success. It must retry with more room and reach the real answer.
func TestTruncatedEmptyReplyRetriesWithMoreRoom(t *testing.T) {
	comp := &capCompleter{responses: []model.CompletionResp{cutOff(""), {Content: "done", FinishReason: "stop", InputTokens: 100}}}
	hist, rec, err := runCapped(t, comp, &fakeExec{}, 1000)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := comp.caps; len(got) != 2 || got[0] != 1000 || got[1] != 2000 {
		t.Errorf("caps = %v, want [1000 2000]", got)
	}
	last := hist[len(hist)-1]
	if last.Content != "done" {
		t.Errorf("last message = %+v", last)
	}
	for _, m := range hist {
		if m.Role == "assistant" && m.Content == "" && len(m.ToolCalls) == 0 {
			t.Error("the empty cut-off reply entered history")
		}
	}
	ends := decodeCallEnds(t, rec)
	if len(ends) != 2 || !ends[0].Discarded || ends[1].Discarded {
		t.Errorf("discarded markers wrong: %+v", ends)
	}
}

// The failure must be an error, because a clean return is what let a run with no
// work done exit 0.
func TestTruncationPastCeilingFailsTheRun(t *testing.T) {
	comp := &capCompleter{responses: []model.CompletionResp{cutOff("")}}
	_, rec, err := runCapped(t, comp, &fakeExec{}, 1000)
	if !errors.Is(err, ErrOutputTruncated) {
		t.Fatalf("err = %v, want ErrOutputTruncated", err)
	}
	if got := comp.caps; len(got) != 3 || got[2] != 4000 {
		t.Errorf("caps = %v, want doubling to the 4x ceiling", got)
	}
	found := false
	for _, e := range rec.events {
		found = found || e.Kind == event.KindError
	}
	if !found {
		t.Error("no error event was recorded")
	}
}

// Partial text must not be mistaken for the final answer, and a retry replaces it.
func TestTruncatedTextIsNotAFinalAnswer(t *testing.T) {
	comp := &capCompleter{responses: []model.CompletionResp{cutOff("half an ans"), {Content: "whole answer", FinishReason: "stop", InputTokens: 100}}}
	hist, _, err := runCapped(t, comp, &fakeExec{}, 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range hist {
		if strings.Contains(m.Content, "half an ans") {
			t.Error("discarded partial text entered history")
		}
	}
	if hist[len(hist)-1].Content != "whole answer" {
		t.Errorf("final = %q", hist[len(hist)-1].Content)
	}
}

// When no retry is left, the human still sees the text, alongside the error.
func TestPartialTextSurvivesFinalFailure(t *testing.T) {
	comp := &capCompleter{responses: []model.CompletionResp{cutOff("partial")}}
	hist, rec, err := runCapped(t, comp, &fakeExec{}, 1000)
	if !errors.Is(err, ErrOutputTruncated) {
		t.Fatalf("err = %v", err)
	}
	if hist[len(hist)-1].Content != "partial" {
		t.Errorf("partial text dropped: %+v", hist[len(hist)-1])
	}
	ends := decodeCallEnds(t, rec)
	if ends[len(ends)-1].Discarded {
		t.Error("kept text is marked discarded, so resume would lose it")
	}
}

// A call cut mid-arguments must never run: a truncated write_file is not what the
// model meant, even when it happens to be repairable.
func TestTruncatedToolCallIsNotExecuted(t *testing.T) {
	bad := model.ToolCall{ID: "1", Name: "write_file", Arguments: `{"path":"a.txt","content":"hal`}
	exec := &fakeExec{}
	comp := &capCompleter{responses: []model.CompletionResp{cutOff("", bad), {Content: "done", FinishReason: "stop", InputTokens: 100}}}
	hist, _, err := runCapped(t, comp, exec, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(exec.calls) != 0 {
		t.Errorf("executed a cut-off call: %v", exec.calls)
	}
	for _, m := range hist {
		if len(m.ToolCalls) > 0 {
			t.Errorf("cut-off call entered history: %+v", m)
		}
	}
}

// Complete tool calls survive a cut-off: the model finished deciding.
func TestTruncatedReplyWithCompleteToolCallsRuns(t *testing.T) {
	good := model.ToolCall{ID: "1", Name: "read_file", Arguments: `{"path":"a.txt"}`}
	exec := &fakeExec{}
	comp := &capCompleter{responses: []model.CompletionResp{cutOff("", good), {Content: "done", FinishReason: "stop", InputTokens: 100}}}
	if _, _, err := runCapped(t, comp, exec, 1000); err != nil {
		t.Fatal(err)
	}
	if len(exec.calls) != 1 {
		t.Errorf("calls = %v, want the complete call to run", exec.calls)
	}
	if comp.caps[1] != 1000 {
		t.Errorf("cap rose to %d for a usable reply", comp.caps[1])
	}
}

// A raised cap stays raised for the run.
func TestRaisedCapIsSticky(t *testing.T) {
	call := model.ToolCall{ID: "1", Name: "read_file", Arguments: `{}`}
	comp := &capCompleter{responses: []model.CompletionResp{
		cutOff(""),
		{ToolCalls: []model.ToolCall{call}, FinishReason: "tool_calls", InputTokens: 100},
		{Content: "done", FinishReason: "stop", InputTokens: 100},
	}}
	if _, _, err := runCapped(t, comp, &fakeExec{}, 1000); err != nil {
		t.Fatal(err)
	}
	if comp.caps[2] != 2000 {
		t.Errorf("caps = %v, want the raise to persist", comp.caps)
	}
}

// Retries must not spend the turn budget, or a low max_turns turns a recoverable
// truncation into ErrMaxTurns.
func TestRetriesDoNotConsumeTurns(t *testing.T) {
	comp := &capCompleter{responses: []model.CompletionResp{cutOff(""), {Content: "done", FinishReason: "stop", InputTokens: 100}}}
	a := New(comp, &fakeExec{}, nopRenderer{}, "m", "", 1, 1000)
	if _, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "t"}}); err != nil {
		t.Fatalf("a single-turn run failed on a retry: %v", err)
	}
}

// Tokens spent on a discarded reply were still paid for.
func TestDiscardedReplyStillCountsUsage(t *testing.T) {
	comp := &capCompleter{responses: []model.CompletionResp{cutOff(""), {Content: "done", FinishReason: "stop", InputTokens: 100, OutputTokens: 5}}}
	a := New(comp, &fakeExec{}, nopRenderer{}, "m", "", 10, 1000)
	total := 0
	a.usageSink = func(in, out int) { total += out }
	if _, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "t"}}); err != nil {
		t.Fatal(err)
	}
	if total != 1005 {
		t.Errorf("output tokens counted = %d, want 1005", total)
	}
}

func TestOutputCeilingIsConfigurable(t *testing.T) {
	// Equal to max_tokens: a hard limit, so no retry at all.
	comp := &capCompleter{responses: []model.CompletionResp{cutOff("")}}
	a := New(comp, &fakeExec{}, nopRenderer{}, "m", "", 10, 1000)
	a.SetOutputCeiling(1000)
	if _, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "t"}}); !errors.Is(err, ErrOutputTruncated) {
		t.Fatalf("err = %v", err)
	}
	if len(comp.caps) != 1 {
		t.Errorf("caps = %v, want one attempt under a hard limit", comp.caps)
	}
	// Above the default factor.
	comp = &capCompleter{responses: []model.CompletionResp{cutOff(""), cutOff(""), cutOff(""), {Content: "done", FinishReason: "stop", InputTokens: 100}}}
	a = New(comp, &fakeExec{}, nopRenderer{}, "m", "", 10, 1000)
	a.SetOutputCeiling(8000)
	if _, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "t"}}); err != nil {
		t.Fatal(err)
	}
	if got := comp.caps[len(comp.caps)-1]; got != 8000 {
		t.Errorf("caps = %v, want to reach 8000", comp.caps)
	}
}

// With no cap sent, the provider's limit applies and there is nothing to raise.
func TestTruncationWithNoCapFailsWithoutRetry(t *testing.T) {
	comp := &capCompleter{responses: []model.CompletionResp{cutOff("")}}
	_, _, err := runCapped(t, comp, &fakeExec{}, 0)
	if !errors.Is(err, ErrOutputTruncated) {
		t.Fatalf("err = %v", err)
	}
	if len(comp.caps) != 1 {
		t.Errorf("caps = %v", comp.caps)
	}
}

// A provider refusing the raised cap ends with the truncation error, not a
// confusing gateway failure.
func TestProviderRefusingRaisedCap(t *testing.T) {
	comp := &capCompleter{
		responses: []model.CompletionResp{cutOff("")},
		errAtCap:  2000,
		err:       errors.New("gateway status 400: max_tokens must be <= 1000"),
	}
	_, _, err := runCapped(t, comp, &fakeExec{}, 1000)
	if !errors.Is(err, ErrOutputTruncated) {
		t.Fatalf("err = %v", err)
	}
}

// A retry cannot ask for more than the window has left.
func TestRaisedCapIsClampedToTheWindow(t *testing.T) {
	comp := &capCompleter{responses: []model.CompletionResp{
		{ToolCalls: []model.ToolCall{{ID: "1", Name: "read_file", Arguments: `{}`}}, FinishReason: "tool_calls", InputTokens: 90_000},
		cutOff(""),
		{Content: "done", FinishReason: "stop", InputTokens: 90_000},
	}}
	a := New(comp, &fakeExec{}, nopRenderer{}, "m", "", 10, 8_000)
	a.ContextWindow = 110_000
	a.SetOutputCeiling(64_000)
	if _, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "t"}}); err != nil {
		t.Fatal(err)
	}
	for _, c := range comp.caps {
		if c > 20_000 {
			t.Errorf("caps = %v, exceeded the room left in the window", comp.caps)
		}
	}
}

// Cleanup starting at 85% must not also be where the run dies.
func TestRunContinuesBetweenPruneAndAbortThresholds(t *testing.T) {
	comp := &capCompleter{responses: []model.CompletionResp{
		{ToolCalls: []model.ToolCall{{ID: "1", Name: "read_file", Arguments: `{}`}}, FinishReason: "tool_calls", InputTokens: 90_000},
		{Content: "done", FinishReason: "stop", InputTokens: 90_000},
	}}
	a := New(comp, &fakeExec{}, nopRenderer{}, "m", "", 10, 1000)
	a.ContextWindow = 100_000
	if _, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "t"}}); err != nil {
		t.Fatalf("a 90%% full window ended the run: %v", err)
	}
}

func TestRunAbortsAboveTheAbortThreshold(t *testing.T) {
	comp := &capCompleter{responses: []model.CompletionResp{
		{ToolCalls: []model.ToolCall{{ID: "1", Name: "read_file", Arguments: `{}`}}, FinishReason: "tool_calls", InputTokens: 96_000},
	}}
	a := New(comp, &fakeExec{}, nopRenderer{}, "m", "", 10, 1000)
	a.ContextWindow = 100_000
	if _, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "t"}}); !errors.Is(err, ErrContextTooLarge) {
		t.Fatalf("err = %v, want ErrContextTooLarge", err)
	}
}

// Over budget after the normal pass, recent tool results give way before the run
// ends. They used to be untouchable until the provider itself refused.
func TestHarderPruneRunsBeforeGivingUp(t *testing.T) {
	big := strings.Repeat("x", 200_000) // ~50k tokens, inside the 25k+ recency protection
	comp := &capCompleter{responses: []model.CompletionResp{{Content: "done", FinishReason: "stop", InputTokens: 100}}}
	a := New(comp, &fakeExec{}, nopRenderer{}, "m", "", 10, 1000)
	a.ContextWindow = 100_000
	hist := []model.Message{
		{Role: "user", Content: "t"},
		{Role: "assistant", ToolCalls: []model.ToolCall{{ID: "1", Name: "read_file", Arguments: `{}`}, {ID: "2", Name: "read_file", Arguments: `{}`}}},
		{Role: "tool", ToolCallID: "1", Name: "read_file", Content: big},
		{Role: "tool", ToolCallID: "2", Name: "read_file", Content: big},
	}
	if _, err := a.Run(context.Background(), hist); err != nil {
		t.Fatalf("run ended with prunable context left: %v", err)
	}
}

// Tool-call arguments were never prunable, so write_file bodies stayed for the
// whole session. This is what filled the window in the recorded failure.
func TestPruneClearsLargeArgsOfStubbedCalls(t *testing.T) {
	body := strings.Repeat("y", 40_000)
	args, _ := json.Marshal(map[string]string{"path": "a.go", "content": body})
	big := strings.Repeat("z", 400_000)
	msgs := []model.Message{
		{Role: "user", Content: "t"},
		{Role: "assistant", ToolCalls: []model.ToolCall{{ID: "old", Name: "write_file", Arguments: string(args)}}},
		{Role: "tool", ToolCallID: "old", Name: "write_file", Content: big},
		{Role: "assistant", ToolCalls: []model.ToolCall{{ID: "new", Name: "write_file", Arguments: string(args)}}},
		{Role: "tool", ToolCallID: "new", Name: "write_file", Content: strings.Repeat("s", 400)},
	}
	shared := msgs[1].ToolCalls
	freed := pruneOldToolResults(msgs, 10, nil, 100)
	got := msgs[1].ToolCalls[0].Arguments
	var fields map[string]any
	if err := json.Unmarshal([]byte(got), &fields); err != nil {
		t.Fatalf("pruned args are not valid JSON: %v", err)
	}
	if fields["path"] != "a.go" {
		t.Errorf("identifying field lost: %v", fields)
	}
	if _, ok := fields["content"]; ok {
		t.Error("large field survived")
	}
	if _, ok := fields[prunedArgsMarker]; !ok {
		t.Error("no marker records what was removed")
	}
	if msgs[3].ToolCalls[0].Arguments != string(args) {
		t.Error("a call whose result is still live lost its arguments")
	}
	if shared[0].Arguments != string(args) {
		t.Error("pruning wrote through a slice shared with the caller")
	}
	if freed < (len(body)+len(big))/bytesPerToken-100 {
		t.Errorf("freed = %d does not count the arguments", freed)
	}
}

func TestPruneLeavesSmallAndNonObjectArgs(t *testing.T) {
	notObject := strings.Repeat("q", 2000)
	small := `{"path":"a.go"}`
	msgs := []model.Message{
		{Role: "assistant", ToolCalls: []model.ToolCall{{ID: "1", Arguments: notObject}, {ID: "2", Arguments: small}}},
		{Role: "tool", ToolCallID: "1", Content: prunedStub},
		{Role: "tool", ToolCallID: "2", Content: prunedStub},
	}
	if freed := pruneOldToolArgs(msgs); freed != 0 {
		t.Errorf("freed = %d", freed)
	}
	if msgs[0].ToolCalls[0].Arguments != notObject || msgs[0].ToolCalls[1].Arguments != small {
		t.Errorf("args changed: %+v", msgs[0].ToolCalls)
	}
}

// With no usage reported, history must stay in the estimate. Treating it as zero
// tokens switched the context guard off.
func TestMissingUsageKeepsContextGuardWorking(t *testing.T) {
	big := strings.Repeat("x", 450_000)
	comp := &capCompleter{responses: []model.CompletionResp{
		{ToolCalls: []model.ToolCall{{ID: "1", Name: "read_file", Arguments: `{}`}}, FinishReason: "tool_calls"},
		{Content: "done", FinishReason: "stop"},
	}}
	a := New(comp, resultExec{big}, nopRenderer{}, "m", "", 10, 1000)
	a.ContextWindow = 100_000
	if _, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "t"}}); !errors.Is(err, ErrContextTooLarge) {
		t.Fatalf("err = %v: ~112k tokens of history against a 100k window must trip the guard", err)
	}
}

type resultExec struct{ out string }

func (resultExec) Schemas() []model.ToolSchema { return nil }
func (r resultExec) Execute(context.Context, string, string) tools.Result {
	return tools.Result{Output: r.out}
}

// A summary cut off once gets a bigger attempt.
func TestSummarizerRetriesWhenCutOff(t *testing.T) {
	comp := &capCompleter{responses: []model.CompletionResp{cutOff(""), {Content: "full summary", FinishReason: "stop"}}}
	s := newSummarizer(comp, "m", 2000)
	out, ok := s.summarize(context.Background(), []model.Message{{Role: "tool", Content: "data"}}, "")
	if !ok || out != "full summary" {
		t.Fatalf("summary = %q, %v", out, ok)
	}
	if comp.caps[1] != 8000 {
		t.Errorf("caps = %v", comp.caps)
	}
}

// A partial summary is better than none, but it must say it is partial.
func TestSummarizerMarksAPartialSummary(t *testing.T) {
	comp := &capCompleter{responses: []model.CompletionResp{cutOff("Objective: x"), cutOff("Objective: x\nDetails: y")}}
	s := newSummarizer(comp, "m", 2000)
	out, ok := s.summarize(context.Background(), []model.Message{{Role: "tool", Content: "data"}}, "")
	if !ok || !strings.Contains(out, "Details: y") || !strings.Contains(out, summaryTruncatedNote) {
		t.Fatalf("summary = %q, %v", out, ok)
	}
}

func TestSummarizerFailsWhenRetryStaysEmpty(t *testing.T) {
	comp := &capCompleter{responses: []model.CompletionResp{cutOff("")}}
	s := newSummarizer(comp, "m", 2000)
	if out, ok := s.summarize(context.Background(), []model.Message{{Role: "tool", Content: "data"}}, ""); ok {
		t.Fatalf("empty summary accepted: %q", out)
	}
}
