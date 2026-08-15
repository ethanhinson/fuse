package runtime

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/event/fsstore"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/tools"
)

// echoTool is a trivial registered tool so a scripted tool call in the round-trip
// test produces a real tool.call + tool.result pair on the durable stream.
type echoTool struct{}

func (echoTool) Name() string               { return "echo" }
func (echoTool) Description() string        { return "echo the args back" }
func (echoTool) Parameters() map[string]any { return map[string]any{} }
func (echoTool) Execute(context.Context, string) tools.Result {
	return tools.Result{Output: "echoed"}
}

// TestReconstructRoundTripEqualsLiveTranscript is D6 test 1 (the correctness core of
// D5): drive a persistent loop for ≥2 turns including a tool call, capture the live
// in-memory transcript the run returns, reconstruct []model.Message purely from that
// loop's durable event stream, and assert BYTE-EQUAL. The reconstruction is fed the
// loop's OWN production event source (a fresh fsstore Replay), not hand-synthesized
// events, per the parity-test discipline.
func TestReconstructRoundTripEqualsLiveTranscript(t *testing.T) {
	// Turn 0: a tool call (echo) then, after the tool result, a terminal text answer
	// (no tools) -> park. Turn 1 (after Send): a terminal text answer -> park.
	fake := newGatedCompleter(
		model.CompletionResp{ToolCalls: []model.ToolCall{{ID: "c1", Name: "echo", Arguments: `{"x":1}`}}}, // turn 0, call 1
		model.CompletionResp{Content: "first answer"},                                                     // turn 0, call 2 -> park
		model.CompletionResp{Content: "second answer"},                                                    // turn 1 after Send -> park
	)

	baseDir := t.TempDir()
	reg := tools.NewRegistry()
	reg.Register(echoTool{})
	rt := New(Deps{
		BaseDir:         baseDir,
		MaxConcurrent:   1,
		NewToolRegistry: func() *tools.Registry { return reg },
		BuildAgent: func(store event.EventStore, tree *agent.AgentTree, modelID string, r *tools.Registry) (*agent.Agent, agent.ChildBuilder, string, error) {
			return agent.New(fake, execAll{r}, nopRenderer{}, modelID, "", 10, 0), nil, modelID, nil
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h, err := rt.StartLoop(ctx, LoopConfig{Task: "hello there", ModelID: "cloud/x", Interactive: true})
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	loopID := h.ID()

	evCh, unsub, err := rt.Observe(ctx, event.DefaultTenant, loopID)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	defer unsub()

	// Turn 0: admit the tool call, then the terminal answer -> park.
	fake.admit(0)
	fake.admit(1)
	waitForKind(t, evCh, event.KindLoopParked, 2*time.Second)

	// Turn 1: Send wakes the loop; admit the follow-up answer -> park again.
	fake.admit(2)
	if err := rt.Send(ctx, event.DefaultTenant, loopID, "and then?"); err != nil {
		t.Fatalf("Send to parked loop: %v", err)
	}
	waitForKind(t, evCh, event.KindLoopParked, 2*time.Second)

	// End the run and capture the live in-memory transcript.
	cancel()
	live, _ := h.Wait()

	// Reconstruct from the loop's OWN durable event stream (fresh Replay).
	store, err := fsstore.NewFSEventStore(baseDir, loopID)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	evs, err := store.Replay(0)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	got, err := reconstructMessages(evs)
	if err != nil {
		t.Fatalf("reconstructMessages: %v", err)
	}

	if !reflect.DeepEqual(got, live) {
		t.Fatalf("reconstructed transcript != live transcript\n got:  %#v\n live: %#v", got, live)
	}
	// Sanity: the transcript must actually carry the multi-turn conversation, not be
	// empty (which would make DeepEqual trivially pass).
	if len(got) < 5 {
		t.Fatalf("expected a multi-turn transcript (user, assistant+call, tool, assistant, user, assistant), got %d messages: %#v", len(got), got)
	}
}
