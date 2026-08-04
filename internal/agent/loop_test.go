package agent

import (
	"context"
	"errors"
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
