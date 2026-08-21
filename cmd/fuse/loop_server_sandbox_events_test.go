package main

import (
	"context"
	"sync"
	"testing"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/tools/sandbox"
)

// sandboxEventRecorder is a minimal per-loop event sink, standing in for the
// StreamKey-bound store the runtime hands BuildAgent.
type sandboxEventRecorder struct {
	mu     sync.Mutex
	events []event.Event
}

func (r *sandboxEventRecorder) Append(e event.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
	return nil
}

func (r *sandboxEventRecorder) Subscribe() (<-chan event.Event, func()) {
	ch := make(chan event.Event)
	close(ch)
	return ch, func() {}
}

func (r *sandboxEventRecorder) Replay(event.Seq) ([]event.Event, error) { return nil, nil }

func (r *sandboxEventRecorder) kinds() []event.Kind {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]event.Kind, 0, len(r.events))
	for _, e := range r.events {
		out = append(out, e.Kind)
	}
	return out
}

// TestLoopServerBindingEmitsSandboxEvents proves the loop-server composition
// root — the one binding that owns a per-loop event store and a loop StreamKey —
// actually installs the pool's emission hooks on the loop's OWN bash tool.
//
// This is the finding the whole-branch review raised: the event kinds, the
// projector's decorateSandbox, the fuse_sandbox_* metric families, the
// fuse-sandbox dashboard, and its alert rules were all unreachable because no
// production caller ever passed hooks. A unit test of the bridge alone would not
// have caught that; only asserting at the binding does.
func TestLoopServerBindingEmitsSandboxEvents(t *testing.T) {
	svc, err := sandbox.NewService(sandbox.Config{Contained: false, Handler: sandbox.HandlerHost})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	// A real registry, because BuildAgent resolves the model alias. No request is
	// ever made: the test drives the tool registry directly, never the loop.
	t.Setenv("LLM_GATEWAY_URL", "http://127.0.0.1:1")
	t.Setenv("LLM_GATEWAY_KEY", "tkn")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config load: %v", err)
	}
	reg := registryFromConfig(cfg)
	alias := reg.Default

	deps := buildLoopServerRuntimeDeps(svc, cfg, reg, alias, defaultToolRegistry(nil, cfg.Research, nil),
		spawnAgentBlock, permissions.AlwaysApprove, nil)
	if deps.NewToolRegistry == nil || deps.BuildAgent == nil {
		t.Fatal("loop-server Deps must build per-loop registries and agents")
	}

	loopReg := deps.NewToolRegistry()
	store := &sandboxEventRecorder{}
	tree := agent.NewAgentTree("root", alias)

	if _, _, _, err := deps.BuildAgent(store, tree, alias, loopReg); err != nil {
		t.Fatalf("BuildAgent: %v", err)
	}

	res := loopReg.Execute(context.Background(), "bash", `{"command":"echo hi"}`)
	if res.IsError {
		t.Fatalf("bash: %s", res.Output)
	}
	if deps.LoopTeardown != nil {
		deps.LoopTeardown(loopReg)
	}

	var sawAcquire, sawRelease bool
	for _, k := range store.kinds() {
		switch k {
		case event.KindSandboxAcquire:
			sawAcquire = true
		case event.KindSandboxRelease, event.KindSandboxReap:
			sawRelease = true
		}
	}
	if !sawAcquire || !sawRelease {
		t.Fatalf("loop-server binding emitted no sandbox lifecycle events (acquire=%v release=%v); kinds=%v — the pool's hooks are not wired at the composition root, so the sandbox projection can never observe data",
			sawAcquire, sawRelease, store.kinds())
	}
}
