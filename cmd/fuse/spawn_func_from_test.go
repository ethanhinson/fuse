package main

import (
	"context"
	"errors"
	"testing"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/tools"
)

// TestSpawnFuncFromReturnsHandle is the change-0044 gate for the composition-root
// adapter: spawnFuncFrom now returns a tools.SpawnHandle (not a pre-collapsed
// string), and its WaitResult() maps the child's agent.SpawnDone{Result,Err} to
// tools.SpawnResult. The async handle stays Go-visible; the tool awaits it.
func TestSpawnFuncFromReturnsHandle(t *testing.T) {
	tree := agent.NewAgentTree("root", "m")
	rootNode := tree.Node(tree.RootID())
	sched := tree.Scheduler()

	spawner := agent.NewSpawner(
		agent.WithTree(tree),
		agent.WithNode(rootNode),
		agent.WithSpawnDepth(0),
		agent.WithChildBuilder(func(ctx context.Context, opts agent.SpawnOpts, _ *agent.AgentNode, _ *agent.AgentTree) (string, error) {
			return "child returned: " + opts.Task, nil
		}),
	)

	fn := spawnFuncFrom(spawner, sched, rootNode)

	handle, err := fn(context.Background(), tools.SpawnRequest{Label: "kid", Task: "compute"})
	if err != nil {
		t.Fatalf("spawnFuncFrom returned error: %v", err)
	}
	if handle == nil {
		t.Fatal("spawnFuncFrom returned a nil handle")
	}
	// Compile-time: the return type is tools.SpawnHandle (interface), proving the
	// seam is handle-returning, not string-returning.
	var _ tools.SpawnHandle = handle

	got := handle.WaitResult()
	if got.Err != nil {
		t.Fatalf("WaitResult err = %v", got.Err)
	}
	if got.Result != "child returned: compute" {
		t.Fatalf("WaitResult Result = %q, want the child's returned string", got.Result)
	}
}

// TestSpawnFuncFromPropagatesChildError: a child that errors surfaces the error
// through the handle's WaitResult, not the spawnFuncFrom start-error path (which
// is reserved for a spawn refused before it started).
func TestSpawnFuncFromPropagatesChildError(t *testing.T) {
	tree := agent.NewAgentTree("root", "m")
	rootNode := tree.Node(tree.RootID())
	sched := tree.Scheduler()

	wantErr := errors.New("child boom")
	spawner := agent.NewSpawner(
		agent.WithTree(tree), agent.WithNode(rootNode), agent.WithSpawnDepth(0),
		agent.WithChildBuilder(func(ctx context.Context, opts agent.SpawnOpts, _ *agent.AgentNode, _ *agent.AgentTree) (string, error) {
			return "", wantErr
		}),
	)
	fn := spawnFuncFrom(spawner, sched, rootNode)
	handle, err := fn(context.Background(), tools.SpawnRequest{Label: "kid", Task: "t"})
	if err != nil {
		t.Fatalf("start error unexpected: %v", err)
	}
	got := handle.WaitResult()
	if got.Err == nil || got.Err.Error() != "child boom" {
		t.Fatalf("WaitResult Err = %v, want 'child boom'", got.Err)
	}
}

// TestSpawnFuncFromWiredToToolPreservesModelContract: the adapter's handle,
// awaited by the real SpawnAgentTool, produces the same model-facing result text
// as before change 0044 — proving the end-to-end cmd → tool → model contract.
func TestSpawnFuncFromWiredToToolPreservesModelContract(t *testing.T) {
	tree := agent.NewAgentTree("root", "m")
	rootNode := tree.Node(tree.RootID())
	sched := tree.Scheduler()

	spawner := agent.NewSpawner(
		agent.WithTree(tree), agent.WithNode(rootNode), agent.WithSpawnDepth(0),
		agent.WithChildBuilder(func(ctx context.Context, opts agent.SpawnOpts, _ *agent.AgentNode, _ *agent.AgentTree) (string, error) {
			return "the child's prose", nil
		}),
	)
	tool := tools.NewSpawnAgentTool(spawnFuncFrom(spawner, sched, rootNode))
	res := tool.Execute(context.Background(), `{"label":"kid","task":"do"}`)
	if res.IsError {
		t.Fatalf("tool errored: %s", res.Output)
	}
	if res.Output != "the child's prose" {
		t.Fatalf("model-facing output = %q, want the child's prose verbatim", res.Output)
	}
}
