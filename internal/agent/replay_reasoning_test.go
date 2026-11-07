package agent

import (
	"context"
	"testing"

	"github.com/ethanhinson/fuse/internal/model"
)

// replayRecorder answers from a script and keeps every request.
type replayRecorder struct {
	scriptedCompleter
	reqs []model.CompletionReq
}

func (r *replayRecorder) Complete(ctx context.Context, req model.CompletionReq) (model.CompletionResp, error) {
	r.reqs = append(r.reqs, req)
	return r.scriptedCompleter.Complete(ctx, req)
}

func runWithReasoning(t *testing.T, replay bool) (*replayRecorder, []model.Message) {
	t.Helper()
	comp := &replayRecorder{scriptedCompleter: scriptedCompleter{responses: []model.CompletionResp{
		{ToolCalls: []model.ToolCall{{ID: "1", Name: "bash", Arguments: `{"command":"ls"}`}}, Reasoning: "list first", ReasoningField: "reasoning"},
		{Content: "all done", Reasoning: "finished"},
	}}}
	a := New(comp, &fakeExec{}, nopRenderer{}, "m", "", 10, 100)
	a.ReplayReasoning = replay
	out, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	return comp, out
}

func assistantAt(t *testing.T, msgs []model.Message, n int) model.Message {
	t.Helper()
	seen := 0
	for _, m := range msgs {
		if m.Role == "assistant" {
			if seen == n {
				return m
			}
			seen++
		}
	}
	t.Fatalf("no assistant message %d in %d messages", n, len(msgs))
	return model.Message{}
}

func TestReplayReasoning_OnKeepsAndSendsIt(t *testing.T) {
	comp, out := runWithReasoning(t, true)
	second := comp.reqs[1]
	if !second.ReplayReasoning {
		t.Fatal("the request must ask the adapter to send reasoning back")
	}
	if m := assistantAt(t, second.Messages, 0); m.Reasoning != "list first" || m.ReasoningField != "reasoning" {
		t.Fatalf("the earlier reply's reasoning must reach the next call, got %+v", m)
	}
	if m := assistantAt(t, out, 1); m.Reasoning != "finished" {
		t.Fatalf("the final reply keeps its reasoning in history, got %+v", m)
	}
}

func TestReplayReasoning_OffDropsIt(t *testing.T) {
	comp, out := runWithReasoning(t, false)
	if comp.reqs[1].ReplayReasoning {
		t.Fatal("replay off must not ask the adapter to send reasoning")
	}
	for _, msgs := range [][]model.Message{comp.reqs[1].Messages, out} {
		for _, m := range msgs {
			if m.Reasoning != "" || m.ReasoningField != "" {
				t.Fatalf("replay off must keep no reasoning in history, got %+v", m)
			}
		}
	}
}

func TestMessagesSize_CountsKeptReasoning(t *testing.T) {
	without := messagesSize([]model.Message{{Role: "assistant", Content: "ab"}})
	with := messagesSize([]model.Message{{Role: "assistant", Content: "ab", Reasoning: "cdef"}})
	if with-without != 4 {
		t.Fatalf("reasoning that will be sent must count toward the context estimate: %d vs %d", with, without)
	}
}
