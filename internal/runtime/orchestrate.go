package runtime

import (
	"context"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/event"
)

// runtimeTarget adapts one loop on this runtime to orchestrator.Target. It reads and
// steers through the runtime's own Attach and Steer, under the orchestrator's tenant,
// so tenant scoping and finished-loop handling are exactly what a client's own
// calls get.
type runtimeTarget struct {
	rt     *inProcRuntime
	tenant event.TenantID
	loopID string
}

func (t *runtimeTarget) Events(ctx context.Context, after event.Seq) ([]event.Event, error) {
	return t.rt.Attach(ctx, t.tenant, t.loopID, after)
}

func (t *runtimeTarget) Steer(ctx context.Context, text string, interrupt bool) error {
	return t.rt.Steer(ctx, t.tenant, t.loopID, text, SteerOpts{Source: agent.SourceOrchestrator, Interrupt: interrupt})
}
