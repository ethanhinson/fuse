package agent

import (
	"context"
	"errors"

	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/tools"
)

// steer.go holds the interrupt half of steering. A plain steer is delivered at the
// next turn boundary (ADR-0022). An Interrupt steer additionally cancels whatever
// the node is doing right now - the in-flight model call or the running tool batch -
// so a long generation or a slow command cannot hold the steer back.

// errSteered is the cancel cause an Interrupt steer puts on the scope it cancels,
// which is how the loop tells "a steer interrupted this" from "the caller cancelled".
var errSteered = errors.New("interrupted by a steering message")

// interruptScope returns a child of ctx that an Interrupt steer for this node
// cancels, and an end func that closes the scope and reports whether a steer was
// what cancelled it. Without a human bus it is a plain child scope.
func (a *Agent) interruptScope(ctx context.Context) (context.Context, func() bool) {
	scope, cancel := context.WithCancelCause(ctx)
	pending := a.humanInjector.setInterrupt(func() { cancel(errSteered) })
	if pending {
		// The interrupt arrived before the hook was registered; honor it now.
		cancel(errSteered)
	}
	return scope, func() bool {
		a.humanInjector.setInterrupt(nil)
		steered := errors.Is(context.Cause(scope), errSteered)
		cancel(nil)
		return steered
	}
}

// runTool runs one tool call. If a steer has interrupted the batch it does not start
// the call - it returns a well-formed skipped result, so every tool call still gets
// its result message and provider call/result pairing stays valid. A call that was
// running when the steer hit has its result marked.
func (a *Agent) runTool(ctx context.Context, turn int, call model.ToolCall) tools.Result {
	if errors.Is(context.Cause(ctx), errSteered) {
		return tools.Result{Output: "[skipped: interrupted by a steering message before this call started]", IsError: true}
	}
	res := a.executeToolBounded(ctx, turn, call)
	if errors.Is(context.Cause(ctx), errSteered) {
		res.Output = "[interrupted by a steering message] " + res.Output
		res.IsError = true
	}
	return res
}
