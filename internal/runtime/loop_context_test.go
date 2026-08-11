package runtime

import (
	"context"
	"testing"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/tools"
)

// ctxKey is a private key a test uses to smuggle a value through Deps.LoopContext
// and observe it at a tool's Execute — standing in for toolidentity.WithPrincipal
// (which the composition root, not the policy-free runtime, supplies).
type ctxKey struct{}

// capturingCtxTool records the context value under ctxKey it is executed with.
type capturingCtxTool struct{ got *string }

func (capturingCtxTool) Name() string               { return "capture" }
func (capturingCtxTool) Description() string        { return "" }
func (capturingCtxTool) Parameters() map[string]any { return map[string]any{} }
func (c capturingCtxTool) Execute(ctx context.Context, _ string) tools.Result {
	if v, ok := ctx.Value(ctxKey{}).(string); ok {
		*c.got = v
	}
	return tools.Result{Output: "ok"}
}

// TestStartLoop_AppliesLoopContext proves the runtime applies Deps.LoopContext to
// the context the loop's run (and thus every tool Execute) derives from — the
// policy-free seam the composition root uses to stamp the authenticated principal
// via toolidentity.WithPrincipal without the runtime importing any auth package
// (ADR-0030). Two loops with DIFFERENT LoopContext-derived values must never cross.
func TestStartLoop_AppliesLoopContext(t *testing.T) {
	run := func(subject string) string {
		var captured string
		reg := tools.NewRegistry()
		reg.Register(capturingCtxTool{got: &captured})

		buildAgent := func(s event.EventStore, tree *agent.AgentTree, modelID string, r *tools.Registry) (*agent.Agent, agent.ChildBuilder, string, error) {
			// A scripted completer that issues ONE tool call to "capture", then stops.
			fake := &scriptedCompleter{responses: []model.CompletionResp{
				{ToolCalls: []model.ToolCall{{ID: "1", Name: "capture", Arguments: "{}"}}},
				{Content: "done"},
			}}
			return agent.New(fake, execAll{r}, nopRenderer{}, modelID, "", 3, 0), nil, modelID, nil
		}

		deps := Deps{
			MaxConcurrent:   1,
			NewToolRegistry: func() *tools.Registry { return reg },
			BuildAgent:      buildAgent,
			// The composition-root hook: stamp the per-loop identity value. The runtime
			// invokes this closure — it never learns loopauth/toolidentity.
			LoopContext: func(ctx context.Context, cfg LoopConfig) context.Context {
				return context.WithValue(ctx, ctxKey{}, cfg.Subject)
			},
		}
		rt := New(deps)
		h, err := rt.StartLoop(context.Background(), LoopConfig{Task: "go", ModelID: "cloud/x", Subject: subject})
		if err != nil {
			t.Fatalf("StartLoop: %v", err)
		}
		if _, err := h.Wait(); err != nil {
			t.Fatalf("Wait: %v", err)
		}
		return captured
	}

	if got := run("alice"); got != "alice" {
		t.Fatalf("loop for alice: tool saw ctx subject %q, want alice", got)
	}
	if got := run("bob"); got != "bob" {
		t.Fatalf("loop for bob: tool saw ctx subject %q, want bob", got)
	}
}
