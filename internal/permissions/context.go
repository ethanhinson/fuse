package permissions

import (
	"context"

	"github.com/ethanhinson/fuse/internal/model"
)

// userMessagesKey is the unexported context key under which the agent loop
// carries the user's conversation turns to the classifier layer. Keeping it a
// distinct unexported struct type prevents collisions with any other package's
// context values.
type userMessagesKey struct{}

// WithUserMessages returns a child context carrying the user's conversation
// turns for the auto-mode classifier to consult. It is the ctx-carry seam the
// agent loop uses to plumb history into the gate WITHOUT widening the
// agent.ToolExecutor interface (which would create an import cycle: permissions
// must never import agent). Storing the turns on the context lets gate.go read
// them at the classifier-layer invocation site, same package, via
// userMessagesFrom. msgs may be nil.
func WithUserMessages(ctx context.Context, msgs []model.Message) context.Context {
	return context.WithValue(ctx, userMessagesKey{}, msgs)
}

// userMessagesFrom returns the user turns carried on ctx by WithUserMessages,
// or nil when none were stored (or the stored value is not the expected type).
// It is nil-safe: a context that never passed through WithUserMessages yields
// nil, preserving today's behavior (the classifier still functions from the
// pending-call turn alone).
func userMessagesFrom(ctx context.Context) []model.Message {
	msgs, _ := ctx.Value(userMessagesKey{}).([]model.Message)
	return msgs
}
