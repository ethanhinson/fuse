package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/tools"
)

// steerFake serves two loops on one runtime: a "target" worker that records every
// request it receives, and an "orch" orchestrator whose replies are scripted.
type steerFake struct {
	mu          sync.Mutex
	targetReqs  []model.CompletionReq
	targetBlock chan struct{} // first target call blocks until cancelled when non-nil
	orchSteps   []model.CompletionResp
	orchCalls   int
	orchSystem  string
	orchTools   []string
}

func (f *steerFake) completer(modelID string) agent.Completer {
	return completerFunc(func(ctx context.Context, req model.CompletionReq) (model.CompletionResp, error) {
		f.mu.Lock()
		if modelID == "orch" {
			defer f.mu.Unlock()
			if f.orchCalls == 0 {
				for _, m := range req.Messages {
					if m.Role == "system" {
						f.orchSystem = m.Content
					}
				}
				for _, s := range req.Tools {
					f.orchTools = append(f.orchTools, s.Name)
				}
			}
			if f.orchCalls < len(f.orchSteps) {
				r := f.orchSteps[f.orchCalls]
				f.orchCalls++
				return r, nil
			}
			return model.CompletionResp{Content: "ok"}, nil
		}
		f.targetReqs = append(f.targetReqs, req)
		first := len(f.targetReqs) == 1
		block := f.targetBlock
		f.mu.Unlock()
		if first && block != nil {
			close(block)
			<-ctx.Done()
			return model.CompletionResp{}, ctx.Err()
		}
		return model.CompletionResp{Content: "target answer"}, nil
	})
}

type completerFunc func(context.Context, model.CompletionReq) (model.CompletionResp, error)

func (f completerFunc) Complete(ctx context.Context, req model.CompletionReq) (model.CompletionResp, error) {
	return f(ctx, req)
}

func newSteerRuntime(t *testing.T, f *steerFake) Runtime {
	t.Helper()
	return New(Deps{
		BaseDir:       t.TempDir(),
		MaxConcurrent: 2,
		IdleTTL:       time.Minute,
		BuildAgent: func(store event.EventStore, tree *agent.AgentTree, modelID string, reg *tools.Registry) (*agent.Agent, agent.ChildBuilder, string, error) {
			// The real bindings wire bash, spawn_agent and friends onto whatever registry
			// they are handed. Do the same here so the test proves the runtime keeps an
			// orchestrator away from them.
			reg.Register(extraTool{"bash"})
			reg.Register(extraTool{"spawn_agent"})
			return agent.New(f.completer(modelID), execAll{reg}, nopRenderer{}, modelID, "binding prompt", 1, 0), nil, modelID, nil
		},
	})
}

type extraTool struct{ name string }

func (e extraTool) Name() string               { return e.name }
func (e extraTool) Description() string        { return "test tool" }
func (e extraTool) Parameters() map[string]any { return map[string]any{"type": "object"} }
func (e extraTool) Execute(context.Context, string) tools.Result {
	return tools.Result{Output: "ran " + e.name}
}

func lastUser(req model.CompletionReq) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			return req.Messages[i].Content
		}
	}
	return ""
}

// The runtime exposes steering as an optional capability: an interrupt steer
// cancels the loop's in-flight model call and the next call sees the labelled steer.
func TestRuntimeSteerInterrupts(t *testing.T) {
	f := &steerFake{targetBlock: make(chan struct{})}
	rt := newSteerRuntime(t, f)
	st, ok := rt.(Steerer)
	if !ok {
		t.Fatal("the in-process runtime must implement Steerer")
	}
	ctx := context.Background()
	h, err := rt.StartLoop(ctx, LoopConfig{Task: "build it", ModelID: "target", Interactive: true})
	if err != nil {
		t.Fatal(err)
	}
	<-f.targetBlock // the first model call is now in flight

	if err := st.Steer(ctx, event.DefaultTenant, h.ID(), "switch to plan B", SteerOpts{Source: agent.SourceOrchestrator, Interrupt: true}); err != nil {
		t.Fatalf("Steer: %v", err)
	}
	eventually(t, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(f.targetReqs) >= 2
	})
	f.mu.Lock()
	got := lastUser(f.targetReqs[1])
	f.mu.Unlock()
	if !strings.Contains(got, "[orchestrator guidance]") || !strings.Contains(got, "switch to plan B") {
		t.Fatalf("second target call saw %q", got)
	}

	if err := st.Steer(ctx, event.DefaultTenant, "nope", "x", SteerOpts{}); err == nil {
		t.Fatal("steering an unknown loop must fail")
	}
}

// StartLoop with Orchestrate starts a chat loop whose tools steer the target loop.
func TestOrchestratorLoopSteersTarget(t *testing.T) {
	f := &steerFake{}
	rt := newSteerRuntime(t, f)
	ctx := context.Background()

	target, err := rt.StartLoop(ctx, LoopConfig{Task: "build it", ModelID: "target", Interactive: true})
	if err != nil {
		t.Fatal(err)
	}
	tev, tunsub, _ := rt.Observe(ctx, event.DefaultTenant, target.ID())
	defer tunsub()
	waitForKind(t, tev, event.KindLoopParked, 3*time.Second)

	args, _ := json.Marshal(map[string]any{"message": "run the linter before you finish"})
	f.mu.Lock()
	f.orchSteps = []model.CompletionResp{
		{ToolCalls: []model.ToolCall{{ID: "c1", Name: "loop_status", Arguments: "{}"}}},
		{ToolCalls: []model.ToolCall{{ID: "c2", Name: "steer_agent", Arguments: string(args)}}},
		{Content: "told it to run the linter"},
	}
	f.mu.Unlock()

	orch, err := rt.StartLoop(ctx, LoopConfig{Task: "keep it honest", ModelID: "orch", Orchestrate: target.ID()})
	if err != nil {
		t.Fatalf("StartLoop(Orchestrate): %v", err)
	}
	oev, ounsub, _ := rt.Observe(ctx, event.DefaultTenant, orch.ID())
	defer ounsub()
	waitForKind(t, oev, event.KindLoopParked, 5*time.Second) // interactive: it parks, it does not finish

	// The target received the steer as orchestrator guidance and took another turn.
	waitForKind(t, tev, event.KindLoopParked, 5*time.Second)
	f.mu.Lock()
	defer f.mu.Unlock()
	if n := len(f.targetReqs); n < 2 {
		t.Fatalf("target never got a second turn (%d calls)", n)
	}
	got := lastUser(f.targetReqs[len(f.targetReqs)-1])
	if !strings.Contains(got, "[orchestrator guidance]") || !strings.Contains(got, "run the linter") {
		t.Fatalf("target saw %q", got)
	}
	if !strings.Contains(f.orchSystem, "orchestrator") {
		t.Fatalf("orchestrator ran without its system prompt: %q", f.orchSystem)
	}
	// It holds only the orchestrator tools - no bash, no spawn_agent, even though the
	// binding wired them onto its registry.
	if strings.Join(f.orchTools, ",") != "loop_status,loop_event,steer_agent" {
		t.Fatalf("orchestrator tools = %v", f.orchTools)
	}
}

func TestOrchestrateRejectsUnknownTarget(t *testing.T) {
	rt := newSteerRuntime(t, &steerFake{})
	if _, err := rt.StartLoop(context.Background(), LoopConfig{Task: "x", ModelID: "orch", Orchestrate: "missing"}); err == nil {
		t.Fatal("orchestrating a loop that does not exist must fail")
	}
}

func eventually(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition never became true")
}

// With BaseDir "" a loop normally writes and keeps no event stream (one-shot stays
// byte-identical). InMemoryEvents makes the stream observable without writing a file.
func TestInMemoryEventsMakeABaseDirlessLoopObservable(t *testing.T) {
	for _, tc := range []struct {
		name      string
		inMemory  bool
		wantAnyEv bool
	}{{"off keeps the no-op store", false, false}, {"on keeps events in memory", true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			f := &steerFake{}
			rt := New(Deps{
				MaxConcurrent:  1,
				InMemoryEvents: tc.inMemory,
				BuildAgent: func(store event.EventStore, tree *agent.AgentTree, modelID string, reg *tools.Registry) (*agent.Agent, agent.ChildBuilder, string, error) {
					return agent.New(f.completer(modelID), execAll{reg}, nopRenderer{}, modelID, "", 2, 0), nil, modelID, nil
				},
			})
			h, err := rt.StartLoop(context.Background(), LoopConfig{Task: "go", ModelID: "target"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := h.Wait(); err != nil {
				t.Fatal(err)
			}
			evs, err := rt.Attach(context.Background(), event.DefaultTenant, h.ID(), 0)
			if err != nil {
				// A finished loop with no store may not even resolve; that is the "off" case.
				if tc.wantAnyEv {
					t.Fatalf("Attach: %v", err)
				}
				return
			}
			if (len(evs) > 0) != tc.wantAnyEv {
				t.Fatalf("events = %d, wantAny = %v", len(evs), tc.wantAnyEv)
			}
		})
	}
}
