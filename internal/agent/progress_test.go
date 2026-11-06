package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/tools"
)

// distinctTurns returns n one-call turns whose arguments differ, so the doom-loop
// detector stays out of the way.
func distinctTurns(name string, n int) []model.CompletionResp {
	out := make([]model.CompletionResp, n)
	for i := range out {
		out[i] = model.CompletionResp{ToolCalls: []model.ToolCall{{ID: "c" + strings.Repeat("x", i+1), Name: name, Arguments: `{"n":"` + strings.Repeat("x", i+1) + `"}`}}}
	}
	return out
}

func nudges(msgs []model.Message) []string {
	var out []string
	for _, m := range msgs {
		if m.Role == "user" && strings.HasPrefix(m.Content, "[no-progress]") {
			out = append(out, m.Content)
		}
	}
	return out
}

func threshold(n int) *int { return &n }

func TestNoProgress_NudgesAtEachInterval(t *testing.T) {
	comp := &scriptedCompleter{responses: distinctTurns("grep", 7)}
	rec := &recordingStore{}
	a := New(comp, &fakeExec{}, nopRenderer{}, "m", "", 50, 100)
	a.SetEventSink(rec)
	a.SetNoProgressCalls(threshold(3))
	msgs, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "go"}})
	if err != nil {
		t.Fatalf("a nudge must not end the run: %v", err)
	}
	got := nudges(msgs)
	if len(got) != 2 {
		t.Fatalf("7 calls at interval 3 should nudge twice, got %d: %v", len(got), got)
	}
	if !strings.Contains(got[0], "3 tool calls") || !strings.Contains(got[1], "6 tool calls") {
		t.Fatalf("nudges should report the running count: %v", got)
	}
	events := 0
	for _, e := range rec.events {
		if e.Kind == event.KindNoProgress {
			events++
		}
	}
	if events != 2 {
		t.Fatalf("each nudge should emit no.progress, got %d", events)
	}
}

func TestNoProgress_FileChangeResetsCount(t *testing.T) {
	turns := distinctTurns("grep", 2)
	turns = append(turns, model.CompletionResp{ToolCalls: []model.ToolCall{{ID: "e", Name: "edit_file", Arguments: `{"e":1}`}}})
	turns = append(turns, distinctTurns("read_file", 2)...)
	a := New(&scriptedCompleter{responses: turns}, &fakeExec{}, nopRenderer{}, "m", "", 50, 100)
	a.SetNoProgressCalls(threshold(3))
	msgs, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "go"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := nudges(msgs); len(got) != 0 {
		t.Fatalf("an edit should restart the count: %v", got)
	}
}

// failingEdit makes every edit_file call fail.
type failingEdit struct{ fakeExec }

func (f *failingEdit) Execute(ctx context.Context, name, args string) tools.Result {
	if name == "edit_file" {
		return tools.Result{IsError: true, Output: "old_string not found"}
	}
	return tools.Result{Output: "ok"}
}

func TestNoProgress_FailedEditIsNotProgress(t *testing.T) {
	a := New(&scriptedCompleter{responses: distinctTurns("edit_file", 3)}, &failingEdit{}, nopRenderer{}, "m", "", 50, 100)
	a.SetNoProgressCalls(threshold(3))
	msgs, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "go"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := nudges(msgs); len(got) != 1 {
		t.Fatalf("failed edits must keep counting, got %d nudges", len(got))
	}
}

func TestNoProgress_ZeroTurnsItOff(t *testing.T) {
	a := New(&scriptedCompleter{responses: distinctTurns("grep", 150)}, &fakeExec{}, nopRenderer{}, "m", "", 0, 100)
	a.SetNoProgressCalls(threshold(0))
	msgs, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "go"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := nudges(msgs); len(got) != 0 {
		t.Fatalf("0 should disable the check: %v", got)
	}
}

func TestNoProgress_OnByDefault(t *testing.T) {
	a := New(&scriptedCompleter{responses: distinctTurns("grep", DefaultNoProgressCalls)}, &fakeExec{}, nopRenderer{}, "m", "", 0, 100)
	msgs, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "go"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := nudges(msgs); len(got) != 1 {
		t.Fatalf("default should nudge once at %d calls, got %d", DefaultNoProgressCalls, len(got))
	}
}

// childSpawningExec runs a child agent for each spawn_agent call, on the context
// the parent's tool call received, the way the Spawner derives a child's context.
type childSpawningExec struct {
	fakeExec
	childCalls int
	childNudge []string
	guard      func(*Agent)
}

func (c *childSpawningExec) Execute(ctx context.Context, name, args string) tools.Result {
	if name != "spawn_agent" {
		return tools.Result{Output: "ok"}
	}
	child := New(&scriptedCompleter{responses: distinctTurns("grep", c.childCalls)}, &fakeExec{}, nopRenderer{}, "m", "", 50, 100)
	c.guard(child)
	msgs, err := child.Run(ctx, []model.Message{{Role: "user", Content: "research"}})
	if err != nil {
		return tools.Result{IsError: true, Output: err.Error()}
	}
	c.childNudge = append(c.childNudge, nudges(msgs)...)
	return tools.Result{Output: "child done"}
}

// TestNoProgress_CountsAcrossChildren pins the run this exists for: every child
// stays under the threshold on its own, and the tree still gets caught.
func TestNoProgress_CountsAcrossChildren(t *testing.T) {
	guard := func(a *Agent) { a.SetNoProgressCalls(threshold(10)) }
	exec := &childSpawningExec{childCalls: 4, guard: guard}
	a := New(&scriptedCompleter{responses: distinctTurns("spawn_agent", 4)}, exec, nopRenderer{}, "m", "", 50, 100)
	guard(a)
	msgs, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "go"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(exec.childNudge) == 0 {
		t.Fatal("a child that crosses the tree-wide threshold should be nudged, though it made only 4 calls itself")
	}
	if got := nudges(msgs); len(got) == 0 {
		t.Fatal("the parent should be nudged too, or it just spawns another child")
	}
}

func TestMaxRunTokens_StopsTheRun(t *testing.T) {
	turns := distinctTurns("grep", 5)
	for i := range turns {
		turns[i].InputTokens, turns[i].OutputTokens = 90, 10
	}
	a := New(&scriptedCompleter{responses: turns}, &fakeExec{}, nopRenderer{}, "m", "", 50, 100)
	a.SetMaxRunTokens(250)
	exec := &fakeExec{}
	a.tools = exec
	_, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "go"}})
	if !errors.Is(err, ErrRunTokenLimit) {
		t.Fatalf("want ErrRunTokenLimit, got %v", err)
	}
	if len(exec.calls) != 2 {
		t.Fatalf("the third response reaches 300 tokens and its tools must not run; ran %d", len(exec.calls))
	}
}

func TestMaxRunTokens_SharedWithChildren(t *testing.T) {
	root := &runProgress{}
	ctx := context.WithValue(context.Background(), runProgressKey{}, root)
	root.tokens.Store(1000)
	turns := distinctTurns("grep", 1)
	turns[0].InputTokens = 1
	child := New(&scriptedCompleter{responses: turns}, &fakeExec{}, nopRenderer{}, "m", "", 50, 100)
	child.SetMaxRunTokens(1000)
	if _, err := child.Run(ctx, []model.Message{{Role: "user", Content: "go"}}); !errors.Is(err, ErrRunTokenLimit) {
		t.Fatalf("a child should stop when the tree is over the cap, got %v", err)
	}
}

func TestMaxRunTokens_OffByDefault(t *testing.T) {
	turns := distinctTurns("grep", 3)
	for i := range turns {
		turns[i].InputTokens = 10_000
	}
	a := New(&scriptedCompleter{responses: turns}, &fakeExec{}, nopRenderer{}, "m", "", 50, 100)
	if _, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "go"}}); err != nil {
		t.Fatalf("no cap configured, got %v", err)
	}
}

func finishChecks(msgs []model.Message) int {
	n := 0
	for _, m := range msgs {
		if m.Role == "user" && m.Content == finishCheckPrompt {
			n++
		}
	}
	return n
}

// toolsExec advertises a tool, as any agent that can do work does.
type toolsExec struct{ fakeExec }

func (*toolsExec) Schemas() []model.ToolSchema { return []model.ToolSchema{{Name: "bash"}} }

// TestFinishCheck_ContinuesARunThatStoppedMidTask is the failure this exists for: the
// model says what it will do next, with no tool call, and then does it when asked.
func TestFinishCheck_ContinuesARunThatStoppedMidTask(t *testing.T) {
	exec := &toolsExec{}
	comp := &scriptedCompleter{responses: []model.CompletionResp{
		{Content: "Let me run the tests now:"},
		{ToolCalls: []model.ToolCall{{ID: "1", Name: "bash", Arguments: `{"command":"go test"}`}}},
		{Content: "Tests pass. Done."},
		{Content: "Done."},
	}}
	a := New(comp, exec, nopRenderer{}, "m", "", 10, 100)
	a.SetFinishCheck(true)
	msgs, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "go"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(exec.calls) != 1 {
		t.Fatalf("the check should get the tests run, tool calls = %v", exec.calls)
	}
	if finishChecks(msgs) != 1 {
		t.Fatalf("want exactly one check over a whole run, got %d", finishChecks(msgs))
	}
	if comp.i != 3 {
		t.Fatalf("the run should end on the reply after the check is spent; %d model calls", comp.i)
	}
}

func TestFinishCheck_SkipsChildren(t *testing.T) {
	ctx, _ := withRunProgress(context.Background())
	comp := &scriptedCompleter{responses: []model.CompletionResp{{Content: "child result"}, {Content: "again"}}}
	a := New(comp, &toolsExec{}, nopRenderer{}, "m", "", 10, 100)
	a.SetFinishCheck(true)
	if _, err := a.Run(ctx, []model.Message{{Role: "user", Content: "go"}}); err != nil {
		t.Fatal(err)
	}
	if comp.i != 1 {
		t.Fatalf("a child's reply is its result; it must not be asked again, got %d calls", comp.i)
	}
}

func TestFinishCheck_CannotLoop(t *testing.T) {
	comp := &scriptedCompleter{responses: []model.CompletionResp{{Content: "done"}, {Content: "done"}, {Content: "done"}}}
	a := New(comp, &toolsExec{}, nopRenderer{}, "m", "", 10, 100)
	a.SetFinishCheck(true)
	if _, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "go"}}); err != nil {
		t.Fatal(err)
	}
	if comp.i != 2 {
		t.Fatalf("want the first reply and one after the check, got %d calls", comp.i)
	}
}

func TestFinishCheck_OffByDefaultInTheAgentAndSkipsToolLess(t *testing.T) {
	comp := &scriptedCompleter{responses: []model.CompletionResp{{Content: "done"}, {Content: "again"}}}
	a := New(comp, &toolsExec{}, nopRenderer{}, "m", "", 10, 100)
	if _, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "go"}}); err != nil {
		t.Fatal(err)
	}
	if comp.i != 1 {
		t.Fatalf("an agent that did not opt in must end on the first reply, got %d calls", comp.i)
	}
	comp = &scriptedCompleter{responses: []model.CompletionResp{{Content: "answer"}, {Content: "again"}}}
	a = New(comp, &fakeExec{}, nopRenderer{}, "m", "", 10, 100)
	a.SetFinishCheck(true)
	if _, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "go"}}); err != nil {
		t.Fatal(err)
	}
	if comp.i != 1 {
		t.Fatalf("an agent with no tools has nothing to continue with, got %d calls", comp.i)
	}
}
