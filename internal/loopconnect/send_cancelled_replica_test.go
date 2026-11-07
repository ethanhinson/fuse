package loopconnect

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/event/fsstore"
	loopv1 "github.com/ethanhinson/fuse/internal/loopwire/v1"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/runtime"
	"github.com/ethanhinson/fuse/internal/tools"
)

type doneCompleter struct{}

func (doneCompleter) Complete(context.Context, model.CompletionReq) (model.CompletionResp, error) {
	return model.CompletionResp{Content: "done"}, nil
}

type noTools struct{}

func (noTools) Schemas() []model.ToolSchema { return nil }
func (noTools) Execute(context.Context, string, string) tools.Result {
	return tools.Result{Output: "ok"}
}

type silentRenderer struct{}

func (silentRenderer) Assistant(string)                {}
func (silentRenderer) ToolCall(string, string)         {}
func (silentRenderer) ToolResult(string, tools.Result) {}
func (silentRenderer) Errorf(string, ...any)           {}
func (silentRenderer) Tokens(int, int)                 {}

// TestSendAfterCancelOnAnotherReplicaSaysCancelled runs two real runtimes over one
// durable store. Replica A finishes the loop and still holds it in memory; replica B
// cancels it. Send through A's handler must then say "loop cancelled", which only
// the durable stream can tell it, since A's own memory says finished.
func TestSendAfterCancelOnAnotherReplicaSaysCancelled(t *testing.T) {
	store := fsstore.NewDurableFSStore(t.TempDir())
	replica := func() runtime.Runtime {
		return runtime.New(runtime.Deps{
			DurableStore:  store,
			Registry:      store,
			MaxConcurrent: 2,
			RelayPoll:     20 * time.Millisecond,
			BuildAgent: func(s event.EventStore, tree *agent.AgentTree, modelID string, reg *tools.Registry) (*agent.Agent, agent.ChildBuilder, string, error) {
				a := agent.New(doneCompleter{}, noTools{}, silentRenderer{}, modelID, "", 5, 100)
				a.SetEventSink(s)
				return a, nil, modelID, nil
			},
		})
	}
	rtA, rtB := replica(), replica()
	ctx := context.Background()

	h, err := rtA.StartLoop(ctx, runtime.LoopConfig{Task: "build it", ModelID: "m"})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := rtA.Send(ctx, event.DefaultTenant, h.ID(), "probe")
		if strings.Contains(strings.ToLower(errString(err)), "finished") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("loop never finished on replica A; last Send: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	if _, err := rtB.(runtime.Canceller).Cancel(ctx, event.DefaultTenant, h.ID(), "stop", "alice"); err != nil {
		t.Fatalf("Cancel on replica B: %v", err)
	}

	client := newTestServer(t, rtA)
	_, err = client.Send(ctx, connect.NewRequest(&loopv1.SendRequest{LoopId: h.ID(), Input: "hello?", Tenant: string(event.DefaultTenant)}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("Send code = %v (%v), want FailedPrecondition", connect.CodeOf(err), err)
	}
	if !strings.Contains(err.Error(), "loop cancelled") {
		t.Fatalf("Send error = %q, want it to say loop cancelled", err)
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
