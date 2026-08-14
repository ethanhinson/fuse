package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/observe"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/probe"
	"github.com/ethanhinson/fuse/internal/runtime"
	"github.com/ethanhinson/fuse/internal/tools"
	"github.com/ethanhinson/fuse/internal/tui"
)

// recordingObserver is the wiring probe used across the change-0061 tests: it
// records every Descriptor it is asked to Start, so a test can assert that a
// given agent (root or child) actually received the session observer rather
// than the hardcoded NoopObserver.
type recordingObserver struct {
	mu    sync.Mutex
	descs []observe.Descriptor
}

func (o *recordingObserver) Start(ctx context.Context, d observe.Descriptor) (context.Context, observe.Handle) {
	o.mu.Lock()
	o.descs = append(o.descs, d)
	o.mu.Unlock()
	return ctx, noopHandle{}
}

func (o *recordingObserver) count() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.descs)
}

type noopHandle struct{}

func (noopHandle) End(observe.Outcome, ...observe.Field) {}

// scriptedGateway installs a deterministic single-turn LLM_GATEWAY_URL double
// (cheap-model shape, never a real provider) and returns the reply it emits.
func scriptedGateway(t *testing.T, reply string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"`+reply+`"}}]}`)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("LLM_GATEWAY_URL", srv.URL)
	t.Setenv("LLM_GATEWAY_KEY", "tkn")
}

// TestBuildAgentCoreInstallsObserver is task 1's seam assertion: the observer
// handed to buildAgentCore is the one the built agent actually observes with.
func TestBuildAgentCoreInstallsObserver(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	scriptedGateway(t, "OBSERVED-REPLY")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config load: %v", err)
	}
	reg := registryFromConfig(cfg)
	toolReg := defaultToolRegistry(cfg.Research, nil)
	rec := &recordingObserver{}

	a, _, err := buildAgentCore(cfg, reg, reg.Default, tui.NewRenderer(io.Discard, false), "", nil, "root",
		toolReg, permissions.AlwaysApprove, nil, false, nil, nil, rec)
	if err != nil {
		t.Fatalf("buildAgentCore: %v", err)
	}
	if _, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if rec.count() == 0 {
		t.Fatal("observer passed to buildAgentCore was never used; agent kept the noop observer")
	}
}

// TestBuildAgentWithRendererAndTraceForwardsObserver covers the wrapper seam.
func TestBuildAgentWithRendererAndTraceForwardsObserver(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	scriptedGateway(t, "OBSERVED-REPLY")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config load: %v", err)
	}
	reg := registryFromConfig(cfg)
	toolReg := defaultToolRegistry(cfg.Research, nil)
	rec := &recordingObserver{}

	a, err := buildAgentWithRendererAndTrace(cfg, reg, reg.Default, tui.NewRenderer(io.Discard, false), false, "",
		toolReg, permissions.AlwaysApprove, nil, "root", nil, false, nil, nil, rec)
	if err != nil {
		t.Fatalf("buildAgentWithRendererAndTrace: %v", err)
	}
	var _ *agent.Agent = a
	if _, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if rec.count() == 0 {
		t.Fatal("observer was not forwarded through buildAgentWithRendererAndTrace")
	}
}

// assertBindingObservesRootAndChild drives one root turn through Deps.BuildAgent
// and one child spawn through the per-loop child-builder it returns, asserting the
// recording observer saw work from BOTH. It is the task-2 shape shared by the three
// local bindings.
func assertBindingObservesRootAndChild(t *testing.T, deps runtime.Deps, rec *recordingObserver,
	tree *agent.AgentTree, alias string, toolReg *tools.Registry) {
	t.Helper()
	a, childBuilder, _, err := deps.BuildAgent(nil, tree, alias, toolReg)
	if err != nil {
		t.Fatalf("BuildAgent: %v", err)
	}
	if _, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatalf("root run: %v", err)
	}
	if rec.count() == 0 {
		t.Fatal("root agent did not use the supplied observer")
	}
	afterRoot := rec.count()
	if childBuilder == nil {
		t.Fatal("BuildAgent returned nil child-builder")
	}
	node := tree.Node(tree.RootID())
	if _, err := childBuilder(context.Background(), agent.SpawnOpts{Label: "child", Task: "hi"}, node, tree); err != nil {
		t.Fatalf("child build: %v", err)
	}
	if rec.count() <= afterRoot {
		t.Fatal("child agent did not use the supplied observer")
	}
}

func TestOneShotBindingHonorsSuppliedObserver(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	scriptedGateway(t, "OBSERVED-REPLY")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config load: %v", err)
	}
	reg := registryFromConfig(cfg)
	toolReg := defaultToolRegistry(cfg.Research, nil)
	tree := agent.NewAgentTreeWithConcurrency(reg.Default, reg.Default, cfg.Agents.MaxConcurrent)
	rec := &recordingObserver{}

	deps, closeMCP := buildOneShotRuntimeDeps(cfg, reg, reg.Default, toolReg, tree, io.Discard, false, nil,
		permissions.AlwaysApprove, "block", false, nil, rec)
	defer closeMCP()
	assertBindingObservesRootAndChild(t, deps, rec, tree, reg.Default, toolReg)
}

func TestResearchProbeBindingHonorsSuppliedObserver(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	scriptedGateway(t, "OBSERVED-REPLY")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config load: %v", err)
	}
	reg := registryFromConfig(cfg)
	toolReg := defaultToolRegistry(cfg.Research, nil)
	tree := agent.NewAgentTreeWithConcurrency(reg.Default, reg.Default, cfg.Agents.MaxConcurrent)
	rec := &recordingObserver{}

	deps := buildResearchProbeRuntimeDeps(researchProbeDepsInput{
		cfg: cfg, reg: reg, alias: reg.Default, toolReg: toolReg, tree: tree,
		act: nil, rootID: tree.RootID(), logSink: probe.NewLog(), traceW: nil,
		rateGate: nil, observer: rec,
	})
	assertBindingObservesRootAndChild(t, deps, rec, tree, reg.Default, toolReg)
}

func TestShellBindingHonorsSuppliedObserver(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	scriptedGateway(t, "OBSERVED-REPLY")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config load: %v", err)
	}
	reg := registryFromConfig(cfg)
	toolReg := defaultToolRegistry(cfg.Research, nil)
	tree := agent.NewAgentTreeWithConcurrency(reg.Default, reg.Default, cfg.Agents.MaxConcurrent)
	rec := &recordingObserver{}

	deps := buildShellRuntimeDeps(shellDepsInput{
		cfg: cfg, reg: reg, alias: reg.Default, toolReg: toolReg, tree: tree,
		verbose: false, skillBlock: "block", childApprove: permissions.AlwaysApprove,
		rootApprove: permissions.AlwaysApprove, observer: rec,
	})
	assertBindingObservesRootAndChild(t, deps, rec, tree, reg.Default, toolReg)
}

// TestBindingsDefaultToNoopObserver is the regression guard for the single most
// important property of change 0061: a caller that supplies no observer (nil) gets
// exactly today's behavior — the built agents keep the noop observer and nothing
// else changes. The recorder is never handed in, so it must stay empty.
func TestBindingsDefaultToNoopObserver(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	scriptedGateway(t, "OBSERVED-REPLY")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config load: %v", err)
	}
	reg := registryFromConfig(cfg)
	for _, tc := range []struct {
		name  string
		build func(toolReg *tools.Registry, tree *agent.AgentTree) runtime.Deps
	}{
		{"one-shot", func(toolReg *tools.Registry, tree *agent.AgentTree) runtime.Deps {
			deps, closeMCP := buildOneShotRuntimeDeps(cfg, reg, reg.Default, toolReg, tree, io.Discard, false, nil,
				permissions.AlwaysApprove, "block", false, nil, nil)
			t.Cleanup(closeMCP)
			return deps
		}},
		{"research-probe", func(toolReg *tools.Registry, tree *agent.AgentTree) runtime.Deps {
			return buildResearchProbeRuntimeDeps(researchProbeDepsInput{
				cfg: cfg, reg: reg, alias: reg.Default, toolReg: toolReg, tree: tree,
				rootID: tree.RootID(), logSink: probe.NewLog(), observer: nil,
			})
		}},
		{"shell", func(toolReg *tools.Registry, tree *agent.AgentTree) runtime.Deps {
			return buildShellRuntimeDeps(shellDepsInput{
				cfg: cfg, reg: reg, alias: reg.Default, toolReg: toolReg, tree: tree,
				skillBlock: "block", childApprove: permissions.AlwaysApprove,
				rootApprove: permissions.AlwaysApprove, observer: nil,
			})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			toolReg := defaultToolRegistry(cfg.Research, nil)
			tree := agent.NewAgentTreeWithConcurrency(reg.Default, reg.Default, cfg.Agents.MaxConcurrent)
			deps := tc.build(toolReg, tree)
			a, childBuilder, _, err := deps.BuildAgent(nil, tree, reg.Default, toolReg)
			if err != nil {
				t.Fatalf("BuildAgent: %v", err)
			}
			if _, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "hi"}}); err != nil {
				t.Fatalf("root run: %v", err)
			}
			node := tree.Node(tree.RootID())
			if _, err := childBuilder(context.Background(), agent.SpawnOpts{Label: "child", Task: "hi"}, node, tree); err != nil {
				t.Fatalf("child build: %v", err)
			}
		})
	}
}
