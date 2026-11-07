package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/observe"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/runtime"
	"github.com/ethanhinson/fuse/internal/tools"
	"github.com/ethanhinson/fuse/internal/tui"
)

// discardRenderer is a no-op agent.Renderer for tests that assert on the wire.
type discardRenderer struct{}

func (discardRenderer) Assistant(string)                {}
func (discardRenderer) ToolCall(string, string)         {}
func (discardRenderer) ToolResult(string, tools.Result) {}
func (discardRenderer) Errorf(string, ...any)           {}
func (discardRenderer) Tokens(int, int)                 {}

// gwNopRenderer discards all loop events; the gateway seam is what this test
// exercises, not the display.
type gwNopRenderer struct{}

func (gwNopRenderer) Assistant(string)                {}
func (gwNopRenderer) ToolCall(string, string)         {}
func (gwNopRenderer) ToolResult(string, tools.Result) {}
func (gwNopRenderer) Errorf(string, ...any)           {}
func (gwNopRenderer) Tokens(int, int)                 {}

// newTestModelRegistry returns a minimal model.Registry sufficient for buildGate
// (which only needs it for auto-mode classifier construction, unused in ModeOff).
func newTestModelRegistry() *model.Registry {
	return &model.Registry{}
}

// nonTestGoFiles returns the package's non-test .go sources, keyed by file name.
func nonTestGoFiles(t *testing.T) map[string]string {
	t.Helper()
	entries, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, f := range entries {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		out[f] = string(src)
	}
	return out
}

// shellTestDeps wires the shell's tools and returns deps whose BuildAgent builds the
// root the way runShell's own build closure does, so a binding test can drive the shell
// the same way as the others. The TUI, not the runtime, drives the real shell.
func shellTestDeps(in shellDepsInput) runtime.Deps {
	wireShellTools(in)
	observer := in.observer
	if observer == nil {
		observer = observe.NoopObserver{}
	}
	sched := in.tree.Scheduler()
	rootNode := in.tree.Node(in.tree.RootID())
	return runtime.Deps{
		Observer:        observer,
		Tree:            in.tree,
		NewToolRegistry: func() *tools.Registry { return in.toolReg },
		BuildAgent: func(_ event.EventStore, _ *agent.AgentTree, _ string, reg2 *tools.Registry) (*agent.Agent, error) {
			a, err := buildAgentWithRendererAndTrace(in.cfg, in.reg, in.alias, tui.NewRenderer(io.Discard, false), in.verbose,
				in.skillBlock, in.cfg.Instructions, reg2, permissions.Human(in.childApprove.Answer), in.traceW, "root",
				in.sessionMode, true, in.rateGate, in.segmentSink, observer)
			if err != nil {
				return nil, err
			}
			a.SetStripSpawn(sched.StripPredicate(rootNode.ID))
			a.SetNodeIdentity(rootNode.ID, rootNode.ParentID, rootNode.Depth)
			return a, nil
		},
	}
}
