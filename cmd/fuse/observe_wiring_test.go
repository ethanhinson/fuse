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
