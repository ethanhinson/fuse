package orchestrator

import (
	"context"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/event"
)

// LocalTarget adapts an agent running in this process - the interactive shell's root
// agent - to Target: it reads the agent's event store and steers through the agent's
// human bus, the same queue a typed message uses.
type LocalTarget struct {
	Store  event.EventStore
	Bus    *agent.HumanBus
	NodeID string
	// BusyFn reports whether the agent is mid-turn. The shell's root agent runs one
	// prompt at a time and is not parked between them, so the event stream alone cannot
	// say "idle". Nil means unknown.
	BusyFn func() bool
}

func (t *LocalTarget) Events(_ context.Context, after event.Seq) ([]event.Event, error) {
	return t.Store.Replay(after)
}

func (t *LocalTarget) Steer(_ context.Context, text string, interrupt bool) error {
	t.Bus.Steer(t.NodeID, agent.SourceOrchestrator, interrupt, text)
	return nil
}

// Busy implements the optional busyReporter.
func (t *LocalTarget) Busy() bool { return t.BusyFn == nil || t.BusyFn() }

// hasBusy is true when BusyFn is wired, so a LocalTarget without it reports nothing.
func (t *LocalTarget) knowsBusy() bool { return t.BusyFn != nil }

// busyReporter is an optional Target capability: a target that knows whether its agent
// is mid-turn says so, and loop_status reports idle accurately.
type busyReporter interface {
	Busy() bool
	knowsBusy() bool
}
