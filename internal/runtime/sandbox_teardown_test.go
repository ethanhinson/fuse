package runtime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/tools"
	"github.com/ethanhinson/fuse/internal/tools/sandbox"
)

// substrateCounter records checkouts against teardowns through the pool's
// emission hooks. It is mutex-protected because the pool's idle reaper fires
// hooks from its own goroutine (learning mutex-test-double-concurrent-provider).
type substrateCounter struct {
	mu        sync.Mutex
	acquired  int
	tornDown  int
	teardowns []sandbox.ReleaseCause
}

func (c *substrateCounter) hooks() sandbox.PoolHooks {
	return sandbox.PoolHooks{
		Acquired: func(sandbox.AcquireInfo) {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.acquired++
		},
		// Reaped — not Released — is the teardown signal: Released means the
		// Runner was handed back and stays WARM (i.e. the container is still
		// alive), so counting it would report a leak as balanced.
		Reaped: func(i sandbox.ReleaseInfo) {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.tornDown++
			c.teardowns = append(c.teardowns, i.Cause)
		},
	}
}

func (c *substrateCounter) read() (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.acquired, c.tornDown
}

// TestStartLoop_SandboxReleasedOnBuildAgentError is the LEAK test.
//
// A loop that fails to start AFTER its tool registry was built has already had
// its bash tool check a Runner out of the warm pool — the pool keeps it alive
// on purpose, which is exactly why an unreleased pool means a container (and
// the pool's reaper goroutine) outlives a loop that never even ran. The
// completion goroutine that normally tears the loop down never launches on this
// path, so Deps.LoopTeardown is the only thing standing between a failed
// StartLoop and a leaked substrate (learning
// per-instance-resource-needs-teardown-on-every-early-return).
//
// A happy-path test cannot see this, and neither can -race: nothing is
// corrupted, something is merely never freed. So the failure path is driven
// directly, and the assertion is made against the pre-teardown state as well as
// the post-teardown one — proving the resource really was outstanding, so the
// test cannot pass vacuously.
func TestStartLoop_SandboxReleasedOnBuildAgentError(t *testing.T) {
	counter := &substrateCounter{}

	// The off-switch substrate, hand-built: a unit test may not require a
	// container daemon, and the host handler's Runners are real enough for the
	// lifecycle this test is about.
	svc, err := sandbox.NewService(sandbox.Config{Contained: false, Handler: sandbox.HandlerHost, IdleTTL: time.Minute})
	if err != nil {
		t.Fatalf("sandbox.NewService: %v", err)
	}
	pool := sandbox.NewPool(svc, sandbox.WithPoolHooks(counter.hooks()))
	// A safety net only: the assertions below are what prove teardown happened.
	defer func() { _ = pool.Close(context.Background()) }()

	loopReg := tools.NewRegistry()
	bash := tools.NewBashWithPool(pool)
	loopReg.Register(bash)

	deps := Deps{
		MaxConcurrent: 1,
		NewToolRegistry: func() *tools.Registry {
			// Stand in for a loop that got far enough to run a command: the
			// warm pool now holds a live Runner for this principal.
			if res := loopReg.Execute(context.Background(), "bash", `{"command":"true"}`); res.IsError {
				t.Errorf("bash setup call failed: %s", res.Output)
			}
			return loopReg
		},
		BuildAgent: func(event.EventStore, *agent.AgentTree, string, *tools.Registry) (*agent.Agent, agent.ChildBuilder, string, error) {
			return nil, nil, "", errors.New("boom")
		},
		LoopTeardown: func(r *tools.Registry) {
			_ = tools.ReleaseSandboxes(context.Background(), r)
		},
	}

	rt := New(deps)
	if _, err := rt.StartLoop(context.Background(), LoopConfig{Task: "go", ModelID: "cloud/x"}); err == nil {
		t.Fatal("StartLoop must return the BuildAgent error")
	}

	acquired, tornDown := counter.read()
	if acquired == 0 {
		t.Fatal("nothing was ever acquired — this test would pass vacuously")
	}
	if acquired != tornDown {
		t.Fatalf("substrate leaked on the early-return path: %d acquired, %d torn down", acquired, tornDown)
	}
}

// The same balance must hold on the ORDINARY completion path, where teardown
// runs on the loop's completion goroutine instead of inline.
func TestStartLoop_SandboxReleasedOnCompletion(t *testing.T) {
	counter := &substrateCounter{}

	svc, err := sandbox.NewService(sandbox.Config{Contained: false, Handler: sandbox.HandlerHost, IdleTTL: time.Minute})
	if err != nil {
		t.Fatalf("sandbox.NewService: %v", err)
	}
	pool := sandbox.NewPool(svc, sandbox.WithPoolHooks(counter.hooks()))
	defer func() { _ = pool.Close(context.Background()) }()

	loopReg := tools.NewRegistry()
	loopReg.Register(tools.NewBashWithPool(pool))

	deps := Deps{
		MaxConcurrent:   1,
		NewToolRegistry: func() *tools.Registry { return loopReg },
		BuildAgent: func(store event.EventStore, tree *agent.AgentTree, _ string, reg *tools.Registry) (*agent.Agent, agent.ChildBuilder, string, error) {
			if res := reg.Execute(context.Background(), "bash", `{"command":"true"}`); res.IsError {
				t.Errorf("bash call failed: %s", res.Output)
			}
			return agent.New(&scriptedCompleter{}, execAll{reg}, nopRenderer{}, "cloud/x", "", 1, 0), nil, "cloud/x", nil
		},
		LoopTeardown: func(r *tools.Registry) {
			_ = tools.ReleaseSandboxes(context.Background(), r)
		},
	}

	rt := New(deps)
	h, err := rt.StartLoop(context.Background(), LoopConfig{Task: "go", ModelID: "cloud/x"})
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	if _, err := h.Wait(); err != nil {
		t.Fatalf("run: %v", err)
	}

	acquired, tornDown := counter.read()
	if acquired == 0 {
		t.Fatal("nothing was ever acquired — this test would pass vacuously")
	}
	if acquired != tornDown {
		t.Fatalf("substrate leaked at loop completion: %d acquired, %d torn down", acquired, tornDown)
	}
}
