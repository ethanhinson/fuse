// Package runtime is the policy-free composition seam over the agent engine. It
// names loop mechanics only — start / send / spawn / observe / attach — and
// composes internal/agent + internal/event + internal/event/fsstore +
// internal/model + internal/tools into one concrete in-process implementation.
//
// Import direction is load-bearing (learning break-import-cycle-with-agent-free-
// subpackage): runtime imports the heavy packages and is imported ONLY by the
// composition root (cmd/fuse). It must NEVER be imported by internal/agent or
// internal/tools — that would form an import cycle. It also imports NO renderer /
// TUI / MCP / CLI type: those are each binding's business and never appear here
// (model + agent + event + tools + fsstore are allowed; a TUI/MCP import is not).
package runtime

import (
	"context"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/model"
)

// Runtime is the policy-free composition seam over the agent engine. It names loop
// mechanics only — no renderer, TUI, approval gate, or MCP/CLI vocabulary appears
// here. Every method keys on loopID = (*agent.AgentTree).RootID(); the interface is
// designed for N loops though this change implements one loop per process (see plan
// Decision Note D-1).
type Runtime interface {
	// StartLoop constructs and drives one agent loop from cfg, returning a handle
	// whose ID() is the tree RootID. It wraps agent.New + SetEventSink/
	// SetNodeIdentity + Agent.Run. The run executes in a goroutine; the handle
	// awaits it.
	StartLoop(ctx context.Context, cfg LoopConfig) (LoopHandle, error)

	// Send injects human input at the loop's next turn boundary (ADR-0022 human-bus).
	// It is tenant-scoped: resolution (cache hit AND durable fall-through) is keyed on
	// (tenant, loopID), so a Send under tenant X never lands on tenant Y's loop. It
	// returns ErrLoopNotFound for an unknown loop and ErrLoopFinished when the loop's
	// run goroutine has already completed (its injector no longer drains, so enqueuing
	// would strand the message); a still-running loop enqueues and returns nil.
	// Single-tenant callers pass event.DefaultTenant (or "" — NormalizeTenant maps it
	// to the default) for byte-identical behavior.
	Send(ctx context.Context, tenant event.TenantID, loopID string, input string) error

	// Spawn starts a child agent under the loop and returns a handle wrapping
	// agent.AgentHandle (ADR-0026: Wait()/Result()/Done).
	Spawn(ctx context.Context, loopID string, opts SpawnOpts) (SpawnHandle, error)

	// Observe returns a live event tail plus an idempotent unsubscribe. It resolves
	// the (tenant, loopID) stream: a locally-running loop subscribes to its own store;
	// a loop known only via the durable registry (started by a prior process/another
	// instance) subscribes to the durable store's cross-instance channel.
	Observe(ctx context.Context, tenant event.TenantID, loopID string) (<-chan event.Event, func(), error)

	// Attach returns durable history with Seq > from for reconnect. It resolves the
	// (tenant, loopID) stream against the durable registry when present, so a cold
	// instance can replay a loop a prior process finished.
	Attach(ctx context.Context, tenant event.TenantID, loopID string, from event.Seq) ([]event.Event, error)

	// Resume revives a persistent conversational session so a client can keep chatting
	// after a disconnect that outlived even the idle-TTL reaper — or on a cold instance
	// that never held the loop (change 0054, D4). It resolves the (tenant, loopID)
	// record tenant-scoped (ADR-0034: a cross-tenant resume is ErrLoopNotFound, never a
	// leak). If the loop is still live here it is a no-op returning the existing handle
	// (resume then just means "re-open your Observe"). If it is finished/evicted, Resume
	// replays the durable event stream, reconstructs the model-facing transcript
	// (single source of truth, D1/D5), builds a fresh agent seeded with it, and re-parks
	// it under a session-scoped context so a subsequent Send drives the next turn instead
	// of returning ErrLoopFinished. It returns ErrLoopNotFound for an unknown loop, and
	// requires the durable Registry + DurableStore (a nil-registry binding has no cross-
	// process identity to resume). Interactive-only: a resumed loop always parks.
	Resume(ctx context.Context, tenant event.TenantID, loopID string) (LoopHandle, error)
}

// LoopConfig is the policy-free description of one loop to start. It carries loop
// mechanics inputs only; renderer/gate/transport are the binding's business and
// never appear here.
type LoopConfig struct {
	Task    string         // the initial user task
	ModelID string         // resolved gateway model id; "" ⇒ the binding resolves the default first
	Tenant  event.TenantID // isolation boundary; empty ⇒ event.DefaultTenant

	// Subject is the authenticated loop-initiator's subject (the `sub` of the
	// per-call delegation token minted at the MCP egress). It is a BARE string here
	// on purpose: the runtime seam must not import an auth package (ADR-0030), so the
	// principal travels as primitives (Tenant + Subject) that the composition root's
	// Deps.LoopContext hook reconstitutes into a loopauth.Principal via
	// toolidentity.WithPrincipal. Empty on the local CLI paths (they supply their own
	// principal through LoopContext instead).
	Subject string

	// Interactive requests persistent conversational mode: the loop parks at each
	// terminal turn boundary awaiting the next Send instead of finishing, so one
	// loop_id carries a multi-turn conversation with server-authoritative history
	// (the transcript is retained in-memory across turns). The loop still ends on
	// context cancellation. Default false ⇒ single-task run-to-completion, so every
	// existing binding is unaffected.
	Interactive bool
}

// LoopHandle observes and awaits one running loop.
type LoopHandle interface {
	ID() string                     // the loop id = tree RootID
	Wait() ([]model.Message, error) // blocks until the loop's Run returns; memoized
}

// SpawnOpts describes one child spawn at the seam (mirrors agent.SpawnOpts' public
// fields without importing agent vocabulary into bindings).
type SpawnOpts struct {
	Label        string
	Task         string
	SystemPrompt string
	ModelID      string
	Tools        []string
	Worker       string
	Expects      any
}

// SpawnHandle wraps agent.AgentHandle (ADR-0026) so a binding awaits a child
// without importing internal/agent's async internals directly at the call site.
type SpawnHandle interface {
	NodeID() string
	Wait() agent.SpawnDone
	Result() (any, error)
}
