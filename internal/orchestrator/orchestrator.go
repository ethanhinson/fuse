// Package orchestrator is the steering side-channel: an ordinary chat loop whose
// tools let it watch another agent loop (the target) and steer it. A human talks to
// the orchestrator at any time, including while the target is mid-turn, and the
// orchestrator reads what the target has been doing and sends it guidance.
//
// The package knows nothing about transports. A Target adapts one loop - through the
// local event store and human bus in the TUI, or through the Runtime seam on a
// server - so the same tools and prompt work in every deployment.
package orchestrator

import (
	"context"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/tools"
)

// Target is the loop the orchestrator watches and steers.
type Target interface {
	// Events returns the target's events with Seq > after, oldest first.
	Events(ctx context.Context, after event.Seq) ([]event.Event, error)
	// Steer queues guidance for the target's root agent. With interrupt set the
	// target's in-flight model call or tool batch is cancelled so the guidance lands
	// now; otherwise it is delivered at the target's next turn boundary.
	Steer(ctx context.Context, text string, interrupt bool) error
}

// ToolNames lists the tools Register adds, in registration order.
var ToolNames = []string{"loop_status", "loop_event", "steer_agent"}

// Register adds the orchestrator tools for target to reg.
func Register(reg *tools.Registry, target Target) {
	w := newWatcher(target)
	reg.Register(&statusTool{w: w})
	reg.Register(&eventTool{w: w})
	reg.Register(&steerTool{t: target})
}
