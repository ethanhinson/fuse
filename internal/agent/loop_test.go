package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/tools"
)

// scriptedCompleter returns queued responses in order.
type scriptedCompleter struct {
	responses []model.CompletionResp
	i         int
}

func (s *scriptedCompleter) Complete(ctx context.Context, req model.CompletionReq) (model.CompletionResp, error) {
	if s.i >= len(s.responses) {
		return model.CompletionResp{Content: "done"}, nil
	}
	r := s.responses[s.i]
	s.i++
	return r, nil
}

// fakeExec records executed tool calls.
type fakeExec struct{ calls []string }

func (f *fakeExec) Schemas() []model.ToolSchema { return nil }
func (f *fakeExec) Execute(ctx context.Context, name, args string) tools.Result {
	f.calls = append(f.calls, name)
	return tools.Result{Output: "ok"}
}

type nopRenderer struct{}

func (nopRenderer) Assistant(string)                {}
func (nopRenderer) ToolCall(string, string)         {}
func (nopRenderer) ToolResult(string, tools.Result) {}
func (nopRenderer) Errorf(string, ...any)           {}
func (nopRenderer) Tokens(int, int)                 {}

func TestRunStopsWhenNoToolCalls(t *testing.T) {
	comp := &scriptedCompleter{responses: []model.CompletionResp{{Content: "final answer"}}}
	exec := &fakeExec{}
	a := New(comp, exec, nopRenderer{}, "m", "", 10, 100)
	hist, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	if hist[len(hist)-1].Content != "final answer" {
		t.Errorf("last message = %q", hist[len(hist)-1].Content)
	}
}

func TestRunExecutesToolThenStops(t *testing.T) {
	comp := &scriptedCompleter{responses: []model.CompletionResp{
		{ToolCalls: []model.ToolCall{{ID: "1", Name: "bash", Arguments: `{"command":"ls"}`}}},
		{Content: "all done"},
	}}
	exec := &fakeExec{}
	a := New(comp, exec, nopRenderer{}, "m", "", 10, 100)
	_, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(exec.calls) != 1 || exec.calls[0] != "bash" {
		t.Errorf("calls = %v", exec.calls)
	}
}

func TestRunLoopDetectionAborts(t *testing.T) {
	same := model.CompletionResp{ToolCalls: []model.ToolCall{{ID: "x", Name: "bash", Arguments: `{"command":"ls"}`}}}
	comp := &scriptedCompleter{responses: []model.CompletionResp{same, same, same, same, same}}
	a := New(comp, &fakeExec{}, nopRenderer{}, "m", "", 50, 100)
	_, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "hi"}})
	if !errors.Is(err, ErrLoopDetected) {
		t.Fatalf("expected ErrLoopDetected, got %v", err)
	}
}

func TestRunMaxTurns(t *testing.T) {
	tool := model.CompletionResp{ToolCalls: []model.ToolCall{{ID: "x", Name: "bash", Arguments: `{"command":"ls"}`}}}
	// distinct args each turn so the loop detector never trips
	var resps []model.CompletionResp
	for i := 0; i < 10; i++ {
		resps = append(resps, model.CompletionResp{ToolCalls: []model.ToolCall{{ID: "x", Name: "bash", Arguments: `{"command":"cmd` + string(rune('a'+i)) + `"}`}}})
	}
	_ = tool
	comp := &scriptedCompleter{responses: resps}
	a := New(comp, &fakeExec{}, nopRenderer{}, "m", "", 3, 100)
	_, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "hi"}})
	if !errors.Is(err, ErrMaxTurns) {
		t.Fatalf("expected ErrMaxTurns, got %v", err)
	}
}

func TestRunSystemPromptInjected(t *testing.T) {
	var seen []model.Message
	comp := &capturingCompleter{onCall: func(req model.CompletionReq) { seen = req.Messages }}
	a := New(comp, &fakeExec{}, nopRenderer{}, "m", "SYS", 5, 100)
	if _, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatal(err)
	}
	if len(seen) == 0 || seen[0].Role != "system" || seen[0].Content != "SYS" {
		t.Fatalf("system prompt not injected first: %+v", seen)
	}
}

type capturingCompleter struct{ onCall func(model.CompletionReq) }

func (c *capturingCompleter) Complete(ctx context.Context, req model.CompletionReq) (model.CompletionResp, error) {
	c.onCall(req)
	return model.CompletionResp{Content: "done"}, nil
}

// TestRunPrunesOldToolResultsWhenOverBudget verifies the loop stubs old tool
// results (never user/assistant messages) instead of failing the turn.
func TestRunPrunesOldToolResultsWhenOverBudget(t *testing.T) {
	comp := &scriptedCompleter{responses: []model.CompletionResp{{Content: "done"}}}
	a := New(comp, &fakeExec{}, nopRenderer{}, "m", "", 10, 100)
	a.ContextWindow = 4000 // budget 3400 tokens; protection = 1000 tokens

	big := strings.Repeat("x", 8000) // ~2000 tokens each
	history := []model.Message{
		{Role: "user", Content: "task"},
		{Role: "assistant", Content: "reading"},
		{Role: "tool", ToolCallID: "1", Name: "read_file", Content: big},
		{Role: "tool", ToolCallID: "2", Name: "read_file", Content: big},
		{Role: "tool", ToolCallID: "3", Name: "read_file", Content: big},
	}
	hist, err := a.Run(context.Background(), history)
	if err != nil {
		t.Fatalf("expected prune + proceed, got %v", err)
	}
	if comp.i == 0 {
		t.Fatal("completer should have been called after pruning")
	}
	if hist[2].Content != prunedStub || hist[3].Content != prunedStub {
		t.Error("older tool results should be stubbed")
	}
	if hist[4].Content != big {
		t.Error("newest tool result within protection budget must survive")
	}
	if hist[0].Content != "task" || hist[1].Content != "reading" {
		t.Error("user/assistant messages must never be pruned")
	}
}

// TestRunErrsWhenPruningInsufficient: un-prunable bloat (user content) still
// ends the turn with ErrContextTooLarge as a last resort.
func TestRunErrsWhenPruningInsufficient(t *testing.T) {
	comp := &scriptedCompleter{responses: []model.CompletionResp{{Content: "unreachable"}}}
	a := New(comp, &fakeExec{}, nopRenderer{}, "m", "", 10, 100)
	a.ContextWindow = 4000
	bloated := []model.Message{
		{Role: "user", Content: strings.Repeat("y", 100_000)}, // ~25k tokens, not prunable
	}
	_, err := a.Run(context.Background(), bloated)
	if !errors.Is(err, ErrContextTooLarge) {
		t.Fatalf("expected ErrContextTooLarge, got %v", err)
	}
	if comp.i != 0 {
		t.Error("gateway must not be called with an oversized context")
	}
}

// lengthRejectingCompleter fails with a context-length error until history
// shrinks below limit, then succeeds — simulates a provider 400.
type lengthRejectingCompleter struct {
	limitBytes int
	calls      int
}

func (c *lengthRejectingCompleter) Complete(_ context.Context, req model.CompletionReq) (model.CompletionResp, error) {
	c.calls++
	if messagesSize(req.Messages) > c.limitBytes {
		return model.CompletionResp{}, errors.New("gateway status 400: maximum context length exceeded")
	}
	return model.CompletionResp{Content: "recovered"}, nil
}

// TestRunRecoversFromProviderLengthRejection: when the estimate fits but the
// provider still rejects, the loop prunes hard and retries exactly once.
func TestRunRecoversFromProviderLengthRejection(t *testing.T) {
	// Recovery protection = min(200k/4, 40k)/4 = 10k tokens = 40KB; each tool
	// result is 15k tokens so only the newest fits inside protection.
	comp := &lengthRejectingCompleter{limitBytes: 70_000}
	a := New(comp, &fakeExec{}, nopRenderer{}, "m", "", 10, 100)
	a.ContextWindow = 200_000 // estimate passes; provider still rejects

	big := strings.Repeat("x", 60_000)
	history := []model.Message{
		{Role: "user", Content: "task"},
		{Role: "tool", ToolCallID: "1", Name: "read_file", Content: big},
		{Role: "tool", ToolCallID: "2", Name: "read_file", Content: big},
	}
	hist, err := a.Run(context.Background(), history)
	if err != nil {
		t.Fatalf("expected recovery, got %v", err)
	}
	if comp.calls != 2 {
		t.Errorf("calls = %d, want 2 (reject then retry)", comp.calls)
	}
	if hist[1].Content != prunedStub {
		t.Error("older tool result should be pruned during recovery")
	}
}
