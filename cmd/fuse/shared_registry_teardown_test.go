package main

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/probe"
	"github.com/ethanhinson/fuse/internal/runtime"
	"github.com/ethanhinson/fuse/internal/tools"
	"github.com/ethanhinson/fuse/internal/tools/sandbox"
)

// sharedPoolCounter counts substrate checkouts against substrate TEARDOWNS
// through the pool's hooks — the observable stand-in for a counting handler,
// since the Handler injection seam (withHostHandler) is unexported and reachable
// only from inside package sandbox. Mutex-guarded because the pool's idle reaper
// fires hooks from its own goroutine (learning
// mutex-test-double-concurrent-provider).
type sharedPoolCounter struct {
	mu       sync.Mutex
	acquired int
	tornDown int
}

func (c *sharedPoolCounter) hooks() sandbox.PoolHooks {
	return sandbox.PoolHooks{
		Acquired: func(sandbox.AcquireInfo) {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.acquired++
		},
		// Reaped — not Released — is the teardown signal: Released means the
		// Runner went back to the pool and stays WARM (the container is still
		// alive), so counting it would report a kill as a normal hand-back.
		Reaped: func(sandbox.ReleaseInfo) {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.tornDown++
		},
	}
}

func (c *sharedPoolCounter) read() (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.acquired, c.tornDown
}

// TestSharedRegistryBindingsDoNotCloseAnotherLoopsSubstrate is the cross-loop
// interference regression.
//
// The shell and research-probe bindings hand EVERY loop the same registry
// (`NewToolRegistry: func() *tools.Registry { return toolReg }`) — and therefore
// the same bash tool and the same warm pool. Pairing that with a
// Deps.LoopTeardown that calls ReleaseSandboxes makes one loop's completion
// close a pool another live loop is still checked out of: the loop-server avoids
// exactly this by rebinding `tools.NewBash(sb)` into its per-loop CLONE, because
// Registry.Clone shares TOOL POINTERS.
//
// -race cannot see this: nothing is corrupted, a live resource is merely killed
// from underneath its owner, who then silently re-opens a fresh one. So the
// interference path is driven explicitly — loop B runs a command, then loop A
// completes — and the assertion is made against the pre-teardown state as well
// as the post-teardown one, so the test cannot pass vacuously.
//
// The invariant under test is NOT "these bindings must have a LoopTeardown"; it
// is "no loop's teardown may close a pool another live loop is using". A binding
// that carries no LoopTeardown at all (releasing its session-scoped pool at
// entrypoint exit instead — see TestEntrypointsReleaseSessionSubstrateAtExit)
// satisfies it by construction, which is why the call below is conditional.
func TestSharedRegistryBindingsDoNotCloseAnotherLoopsSubstrate(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := config.Default()
	reg := model.DefaultRegistry()

	cases := []struct {
		name  string
		build func(toolReg *tools.Registry, tree *agent.AgentTree) runtime.Deps
	}{
		{"shell", func(toolReg *tools.Registry, tree *agent.AgentTree) runtime.Deps {
			return buildShellRuntimeDeps(shellDepsInput{
				cfg: cfg, reg: reg, alias: reg.Default, toolReg: toolReg, tree: tree,
				skillBlock:   "block",
				childApprove: permissions.AlwaysApprove,
				rootApprove:  permissions.AlwaysApprove,
			})
		}},
		{"research-probe", func(toolReg *tools.Registry, tree *agent.AgentTree) runtime.Deps {
			return buildResearchProbeRuntimeDeps(researchProbeDepsInput{
				cfg: cfg, reg: reg, alias: reg.Default, toolReg: toolReg, tree: tree,
				rootID: tree.RootID(), logSink: probe.NewLog(),
			})
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			counter := &sharedPoolCounter{}
			// The off-switch substrate, hand-built: a unit test may not require a
			// container daemon, and the host handler's Runners are real enough for
			// the lifecycle this test is about. A long IdleTTL keeps the reaper out
			// of the assertions.
			svc, err := sandbox.NewService(sandbox.Config{Contained: false, Handler: sandbox.HandlerHost, IdleTTL: time.Minute})
			if err != nil {
				t.Fatalf("sandbox.NewService: %v", err)
			}
			pool := sandbox.NewPool(svc, sandbox.WithPoolHooks(counter.hooks()))
			// A safety net only: the assertions below are what prove the state.
			defer func() { _ = pool.Close(context.Background()) }()

			toolReg := defaultToolRegistry(nil, cfg.Research, nil)
			// Overwrite the substrate-less default bash by name with one over the
			// counted pool: this is the tool pointer both "loops" will share.
			toolReg.Register(tools.NewBashWithPool(pool))
			tree := agent.NewAgentTreeWithConcurrency(reg.Default, reg.Default, cfg.Agents.MaxConcurrent)

			deps := tc.build(toolReg, tree)
			if deps.NewToolRegistry == nil {
				t.Fatal("binding supplies no NewToolRegistry")
			}

			// Two loops over the ONE binding.
			regA := deps.NewToolRegistry()
			regB := deps.NewToolRegistry()

			// Loop B runs a command: the warm pool now holds a LIVE Runner for it.
			if res := regB.Execute(context.Background(), "bash", `{"command":"true"}`); res.IsError {
				t.Fatalf("loop B bash: %s", res.Output)
			}
			acquired, tornDown := counter.read()
			if acquired == 0 {
				t.Fatal("nothing was ever acquired — this test would pass vacuously")
			}
			if tornDown != 0 {
				t.Fatalf("loop B's substrate was torn down before loop A even completed (%d reaped)", tornDown)
			}

			// Loop A completes FIRST, while loop B is still running.
			if deps.LoopTeardown != nil {
				deps.LoopTeardown(regA)
			}

			if _, tornDown = counter.read(); tornDown != 0 {
				t.Errorf("loop A's teardown reaped %d of loop B's live Runners — one loop's completion must never close another live loop's pool", tornDown)
			}
			// And loop B must still be able to run on its substrate.
			if res := regB.Execute(context.Background(), "bash", `{"command":"true"}`); res.IsError {
				t.Errorf("loop B's substrate is unusable after loop A's teardown: %s", res.Output)
			}
		})
	}
}

// TestEntrypointsReleaseSessionSubstrateAtExit is the other half of that fix.
//
// Dropping LoopTeardown from the two shared-registry bindings only holds if
// SOMETHING still releases the session's warm pool — otherwise the loop-scoped
// leak simply becomes a process-scoped one, which is invisible today (`run --rm`
// is per-Exec) and a stranded container the moment a persistent-container
// substrate lands. The release belongs at the entrypoint that OWNS the shared
// registry, deferred so it covers every early return.
//
// A source-text guard, in the style of TestNoDirectEngineDriveAtCmdSites: the
// shell's release happens after a bubbletea session and the probe's after a live
// provider run, neither of which a unit test can drive with an injected pool.
func TestEntrypointsReleaseSessionSubstrateAtExit(t *testing.T) {
	const want = "defer func() { _ = tools.ReleaseSandboxes(context.Background(), toolReg) }()"
	for _, f := range []string{"shell.go", "research_probe.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v — a binding entrypoint was renamed/moved", f, err)
		}
		if !strings.Contains(string(b), want) {
			t.Errorf("%s does not release its session-scoped sandbox pool at exit; expected %q right after the session tool registry is built", f, want)
		}
	}
}
