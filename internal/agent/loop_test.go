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

// TestRunLoopDetectionForcesApprovalWhenInteractive verifies that with a loop
// approval hook set (interactive posture), a tripped detector routes the
// repeated call through approval with a "possible loop" preview instead of
// aborting; on approval the run continues, on rejection it aborts. (0038)
func TestRunLoopDetectionForcesApprovalWhenInteractive(t *testing.T) {
	same := model.CompletionResp{ToolCalls: []model.ToolCall{{ID: "x", Name: "bash", Arguments: `{"command":"ls"}`}}}

	t.Run("approve continues past the trip", func(t *testing.T) {
		// 3 identical calls trip the detector; on approval the run continues,
		// then a natural stop ends it. Assert approval was consulted and the
		// preview names the repeated call.
		comp := &scriptedCompleter{responses: []model.CompletionResp{same, same, same, {Content: "done"}}}
		var previews []string
		a := New(comp, &fakeExec{}, nopRenderer{}, "m", "", 50, 100)
		a.LoopApproval = func(ctx context.Context, preview string) (bool, error) {
			previews = append(previews, preview)
			return true, nil
		}
		_, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "hi"}})
		if err != nil {
			t.Fatalf("approved possible-loop should continue, got %v", err)
		}
		if len(previews) == 0 {
			t.Fatal("loop approval hook was never consulted on the trip")
		}
		if !strings.Contains(previews[0], "bash") {
			t.Errorf("preview %q should name the repeated tool call", previews[0])
		}
	})

	t.Run("reject aborts with ErrLoopDetected", func(t *testing.T) {
		comp := &scriptedCompleter{responses: []model.CompletionResp{same, same, same, same}}
		a := New(comp, &fakeExec{}, nopRenderer{}, "m", "", 50, 100)
		a.LoopApproval = func(ctx context.Context, preview string) (bool, error) {
			return false, nil
		}
		_, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "hi"}})
		if !errors.Is(err, ErrLoopDetected) {
			t.Fatalf("rejected possible-loop should abort with ErrLoopDetected, got %v", err)
		}
	})
}

// TestRunLoopDetectionAbortsWhenNonInteractive verifies the non-interactive
// posture (no approval hook): a tripped detector aborts with an ErrLoopDetected
// whose message names the repeated call. (0038)
func TestRunLoopDetectionAbortsWhenNonInteractive(t *testing.T) {
	same := model.CompletionResp{ToolCalls: []model.ToolCall{{ID: "x", Name: "web_search", Arguments: `{"q":"x"}`}}}
	comp := &scriptedCompleter{responses: []model.CompletionResp{same, same, same, same}}
	a := New(comp, &fakeExec{}, nopRenderer{}, "m", "", 50, 100) // no LoopApproval
	_, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "hi"}})
	if !errors.Is(err, ErrLoopDetected) {
		t.Fatalf("expected ErrLoopDetected, got %v", err)
	}
	if !strings.Contains(err.Error(), "web_search") {
		t.Errorf("abort error %q should name the repeated tool call", err.Error())
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

// TestRunUnlimitedTurnsWhenZero verifies maxTurns==0 is the unlimited
// sentinel: the loop runs well past the retired 25-turn default without ever
// returning ErrMaxTurns, stopping only when the model stops calling tools. (0038)
func TestRunUnlimitedTurnsWhenZero(t *testing.T) {
	var resps []model.CompletionResp
	// 30 distinct tool-call turns (distinct args so the loop detector never
	// trips), then a natural stop — well past the old 25 cap.
	for i := 0; i < 30; i++ {
		resps = append(resps, model.CompletionResp{ToolCalls: []model.ToolCall{
			{ID: "x", Name: "bash", Arguments: `{"command":"cmd` + strings.Repeat("z", i) + `"}`},
		}})
	}
	resps = append(resps, model.CompletionResp{Content: "finally done"})
	comp := &scriptedCompleter{responses: resps}
	exec := &fakeExec{}
	a := New(comp, exec, nopRenderer{}, "m", "", 0, 100) // 0 = unlimited
	_, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("maxTurns==0 must be unlimited, got %v", err)
	}
	if len(exec.calls) != 30 {
		t.Errorf("executed %d tool calls, want 30 (ran past the retired 25 cap)", len(exec.calls))
	}
}

// TestRunNoLongerCoercesZeroTo25 guards the retirement of the agent.New
// `<=0 ⇒ 25` coercion: a zero maxTurns must not silently become a 25-turn
// cap. (0038)
func TestRunNoLongerCoercesZeroTo25(t *testing.T) {
	var resps []model.CompletionResp
	for i := 0; i < 26; i++ {
		resps = append(resps, model.CompletionResp{ToolCalls: []model.ToolCall{
			{ID: "x", Name: "bash", Arguments: `{"command":"c` + strings.Repeat("q", i) + `"}`},
		}})
	}
	resps = append(resps, model.CompletionResp{Content: "done"})
	comp := &scriptedCompleter{responses: resps}
	a := New(comp, &fakeExec{}, nopRenderer{}, "m", "", 0, 100)
	if _, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatalf("zero maxTurns should not cap at 25, got %v", err)
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
