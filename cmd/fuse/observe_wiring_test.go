package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"unsafe"

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

// sawKind reports whether the observer was asked to Start a span of kind k.
// Kind matters where mere count does not: the runtime emits its own
// OperationLoop span from Deps.Observer before any agent exists, so only an
// agent-layer kind (OperationModelAttempt) proves the observer reached the
// root agent.
func (o *recordingObserver) sawKind(k observe.OperationKind) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, d := range o.descs {
		if d.Kind == k {
			return true
		}
	}
	return false
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

// observerOf reads the observer an agent actually carries. Agent.observer is
// deliberately unexported and change 0061 keeps it that way — no production
// getter exists purely so a test can look. A package-local export_test.go seam
// in internal/agent cannot help here (it is visible only to that package's own
// test binary, not to package main), so this wiring-only reader reaches the
// field through reflect instead of widening the production API.
func observerOf(t *testing.T, a *agent.Agent) observe.Observer {
	t.Helper()
	f := reflect.ValueOf(a).Elem().FieldByName("observer")
	if !f.IsValid() {
		t.Fatal("agent.Agent has no observer field; update observerOf")
	}
	o, ok := reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem().Interface().(observe.Observer)
	if !ok {
		t.Fatalf("agent observer field is %s, not an observe.Observer", f.Type())
	}
	return o
}

// assertNoop fails unless o is exactly the hardcoded no-op observer.
func assertNoop(t *testing.T, what string, o observe.Observer) {
	t.Helper()
	if _, ok := o.(observe.NoopObserver); !ok {
		t.Fatalf("%s = %T, want observe.NoopObserver: an empty observability config "+
			"must stay byte-identical to pre-0061 behavior and never install a live observer", what, o)
	}
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
	// Deps.Observer is load-bearing on its own, and calling BuildAgent directly
	// cannot see it: internal/runtime/inproc.go OVERWRITES whatever BuildAgent
	// installed with a.SetObserver(r.deps.Observer) on the StartLoop path. A
	// binding that wires the observer only through its BuildAgent closure would
	// satisfy every assertion below and still lose the observer in a real run, so
	// assert the field identity explicitly. TestOneShotBindingObserverSurvivesStartLoop
	// proves the same property end-to-end through the real path.
	if deps.Observer != observe.Observer(rec) {
		t.Fatalf("Deps.Observer = %#v, want the supplied observer: runtime/inproc.go overwrites the "+
			"root agent's observer with Deps.Observer, so a binding that publishes it only via "+
			"BuildAgent loses it on the StartLoop path", deps.Observer)
	}
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

// TestOneShotBindingObserverSurvivesStartLoop drives a local binding through the
// REAL entry-point path — runtime.New(deps).StartLoop — rather than calling
// deps.BuildAgent directly. This is the only test in this file that exercises
// internal/runtime/inproc.go's a.SetObserver(r.deps.Observer), which discards the
// observer BuildAgent installed and re-installs Deps.Observer. Asserting on the
// agent-layer OperationModelAttempt span (not merely on span count) keeps the
// runtime's own fuse.loop.run span from satisfying it.
func TestOneShotBindingObserverSurvivesStartLoop(t *testing.T) {
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

	h, err := runtime.New(deps).StartLoop(context.Background(), runtime.LoopConfig{Task: "hi", ModelID: reg.Default})
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	if _, err := h.Wait(); err != nil {
		t.Fatalf("loop wait: %v", err)
	}
	if !rec.sawKind(observe.OperationModelAttempt) {
		t.Fatalf("no agent-layer %q span reached the supplied observer through StartLoop; "+
			"runtime/inproc.go overwrote the root agent's observer with Deps.Observer",
			observe.OperationModelAttempt)
	}
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
// else changes.
//
// It asserts both places a live observer could leak into a binding: Deps.Observer
// (the load-bearing field, because runtime/inproc.go overwrites whatever
// BuildAgent installed with it on the StartLoop path) and the root agent the
// binding builds. Each binding's child-builder closes over the same substituted
// observer value, and the constructors it calls are guarded directly by
// TestChildConstructorsNilObserverStaysNoop below — a ChildBuilder returns only
// the child's result string, so the child agent itself is not reachable here.
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
			assertNoop(t, "Deps.Observer", deps.Observer)
			a, childBuilder, _, err := deps.BuildAgent(nil, tree, reg.Default, toolReg)
			if err != nil {
				t.Fatalf("BuildAgent: %v", err)
			}
			assertNoop(t, "root agent observer", observerOf(t, a))
			if _, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "hi"}}); err != nil {
				t.Fatalf("root run: %v", err)
			}
			// Running must not swap the observer out from under the agent either.
			assertNoop(t, "root agent observer after Run", observerOf(t, a))
			node := tree.Node(tree.RootID())
			if _, err := childBuilder(context.Background(), agent.SpawnOpts{Label: "child", Task: "hi"}, node, tree); err != nil {
				t.Fatalf("child build: %v", err)
			}
		})
	}
}

// TestChildConstructorsNilObserverStaysNoop guards the child half of the same
// property: every local binding's child-builder reaches an agent through one of
// these two constructors, so a nil observer must leave the built child on the
// no-op observer.
func TestChildConstructorsNilObserverStaysNoop(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	scriptedGateway(t, "OBSERVED-REPLY")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config load: %v", err)
	}
	reg := registryFromConfig(cfg)
	toolReg := defaultToolRegistry(cfg.Research, nil)
	r := tui.NewRenderer(io.Discard, false)

	core, _, err := buildAgentCore(cfg, reg, reg.Default, r, "", nil, "child",
		toolReg, permissions.AlwaysApprove, nil, false, nil, nil, nil)
	if err != nil {
		t.Fatalf("buildAgentCore: %v", err)
	}
	assertNoop(t, "buildAgentCore(nil observer) agent observer", observerOf(t, core))

	wrapped, err := buildAgentWithRendererAndTrace(cfg, reg, reg.Default, r, false, "",
		toolReg, permissions.AlwaysApprove, nil, "child", nil, false, nil, nil, nil)
	if err != nil {
		t.Fatalf("buildAgentWithRendererAndTrace: %v", err)
	}
	assertNoop(t, "buildAgentWithRendererAndTrace(nil observer) agent observer", observerOf(t, wrapped))
}
