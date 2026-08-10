package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/event/fsstore"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/tools"
)

// ErrLoopNotFound is returned by Observe/Attach/Send/Spawn when the loopID is not
// a known, started loop on this Runtime.
var ErrLoopNotFound = errors.New("runtime: loop not found")

// Deps are the binding-supplied collaborators the Runtime composes. None are
// renderer/TUI/gate types; a binding wires those around the Runtime, not into it.
type Deps struct {
	// BuildAgent constructs the root *agent.Agent for a loop from its resolved
	// model id and the per-loop tool registry, using the binding's own completer/
	// tools/gate wiring. It is the one seam through which a binding injects its
	// engine construction WITHOUT the Runtime importing cmd-layer builders. Returns
	// the agent and the resolved gateway model id.
	BuildAgent func(modelID string, toolReg *tools.Registry) (*agent.Agent, string, error)
	// BuildChild runs a local child agent for a Spawn (agent.ChildBuilder). It is
	// loop-specific policy the binding supplies; a binding still owns what the child
	// renders/gates INSIDE that closure. agent.ChildBuilder is an agent type, not a
	// renderer/TUI/MCP type, so it does not break the policy-free constraint. May be
	// nil (Spawn then runs the child with no builder — the child produces no result).
	BuildChild agent.ChildBuilder
	// NewToolRegistry builds the per-loop tool registry (spawn_agent etc. are wired
	// by the binding via BuildAgent's closure over the same registry). May be nil,
	// in which case an empty registry is used.
	NewToolRegistry func() *tools.Registry
	// BaseDir is where per-loop fsstore event logs live (e.g. session.DefaultLogDir()).
	BaseDir string
	// MaxConcurrent seeds the loop tree's scheduler concurrency.
	MaxConcurrent int
}

// inProcRuntime is the concrete in-process Runtime. It owns each loop's event
// store as instance state (Decision Note D-1): it opens the *fsstore.FSEventStore
// per loop and holds it on the loopID-keyed loop value.
type inProcRuntime struct {
	deps  Deps
	mu    sync.Mutex
	loops map[string]*loop
}

// loop holds per-loopID state: the tree, the root node, the event store, the human
// bus, and the loop handle. It is the loopID-keyed map value that makes the N-loop
// design real while only one is populated per process.
type loop struct {
	id         string
	tree       *agent.AgentTree
	node       *agent.AgentNode
	store      event.EventStore
	humanBus   *agent.HumanBus
	buildChild agent.ChildBuilder
	handle     *loopHandle
}

// New builds an in-process Runtime. deps carries the collaborators a binding
// constructs (model Completer/agent factory, tool registry factory, config) so the
// Runtime stays free of cmd-layer policy. The event store is opened per-loop under
// deps.BaseDir/<rootID> at StartLoop time.
func New(deps Deps) *inProcRuntime {
	return &inProcRuntime{deps: deps, loops: map[string]*loop{}}
}

// StartLoop constructs and drives one agent loop from cfg (see the Runtime
// interface). It wraps agent.New (via deps.BuildAgent) + SetEventSink/
// SetNodeIdentity + Agent.Run; the run executes in a goroutine and the returned
// handle awaits it.
func (r *inProcRuntime) StartLoop(ctx context.Context, cfg LoopConfig) (LoopHandle, error) {
	tree := agent.NewAgentTreeWithConcurrency(cfg.ModelID, cfg.ModelID, r.deps.MaxConcurrent)
	rootID := tree.RootID()
	rootNode := tree.Node(rootID)

	store, err := fsstore.NewFSEventStore(r.deps.BaseDir, rootID)
	if err != nil {
		return nil, fmt.Errorf("runtime: open event store: %w", err)
	}

	var toolReg *tools.Registry
	if r.deps.NewToolRegistry != nil {
		toolReg = r.deps.NewToolRegistry()
	} else {
		toolReg = tools.NewRegistry()
	}

	a, _, err := r.deps.BuildAgent(cfg.ModelID, toolReg)
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("runtime: build agent: %w", err)
	}
	a.SetEventSink(store)
	a.SetNodeIdentity(rootNode.ID, rootNode.ParentID, rootNode.Depth)

	lp := &loop{
		id:         rootID,
		tree:       tree,
		node:       rootNode,
		store:      store,
		humanBus:   agent.NewHumanBus(tree),
		buildChild: r.deps.BuildChild,
	}
	r.mu.Lock()
	if r.loops == nil {
		r.loops = map[string]*loop{}
	}
	r.loops[rootID] = lp
	r.mu.Unlock()

	// Root human-message injector so Send() lands at a turn boundary (ADR-0022).
	a.SetHumanInjector(agent.NewHumanInjector(rootNode.ID, lp.humanBus))

	h := &loopHandle{id: rootID, done: make(chan struct{})}
	lp.handle = h
	go func() {
		defer close(h.done)
		msgs, rerr := a.Run(ctx, []model.Message{{Role: "user", Content: cfg.Task}})
		h.msgs, h.err = msgs, rerr
	}()
	return h, nil
}

// loopHandle observes and awaits one running loop. Wait is memoized: done is closed
// exactly once by the run goroutine, after which msgs/err are stable.
type loopHandle struct {
	id   string
	done chan struct{}
	msgs []model.Message
	err  error
}

// ID returns the loop id (the tree RootID).
func (h *loopHandle) ID() string { return h.id }

// Wait blocks until the loop's Run returns and yields its final messages + error.
// It is memoized via the closed done channel — callable any number of times.
func (h *loopHandle) Wait() ([]model.Message, error) {
	<-h.done
	return h.msgs, h.err
}

// lookup returns the loop for loopID under the mutex, or ErrLoopNotFound.
func (r *inProcRuntime) lookup(loopID string) (*loop, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	lp, ok := r.loops[loopID]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrLoopNotFound, loopID)
	}
	return lp, nil
}

// Observe returns a live event tail plus an idempotent unsubscribe
// (EventStore.Subscribe). Delivery is non-blocking end to end (ADR-0016/0025): a
// slow observer drops the newest event with a gap rather than wedging the loop.
func (r *inProcRuntime) Observe(loopID string) (<-chan event.Event, func(), error) {
	lp, err := r.lookup(loopID)
	if err != nil {
		return nil, nil, err
	}
	ch, cancel := lp.store.Subscribe()
	return ch, cancel, nil
}

// Attach returns durable history with Seq > from (EventStore.Replay) for reconnect.
func (r *inProcRuntime) Attach(loopID string, from event.Seq) ([]event.Event, error) {
	lp, err := r.lookup(loopID)
	if err != nil {
		return nil, err
	}
	return lp.store.Replay(from)
}

// Send injects human input at the loop's next turn boundary (ADR-0022 human-bus).
// It enqueues for the root node; the root injector installed at StartLoop drains it
// at the next turn boundary. ModeRespond = a direct reply to that node (not a
// broadcast).
func (r *inProcRuntime) Send(ctx context.Context, loopID string, input string) error {
	lp, err := r.lookup(loopID)
	if err != nil {
		return err
	}
	lp.humanBus.Enqueue(lp.node.ID, agent.ModeRespond, "@human", input)
	return nil
}

// Spawn starts a child agent under the loop and returns a handle wrapping
// agent.AgentHandle (ADR-0026). The Spawner emits spawn.start/spawn.done onto the
// loop's own event stream so an observer sees child lifecycle without the handle.
func (r *inProcRuntime) Spawn(ctx context.Context, loopID string, opts SpawnOpts) (SpawnHandle, error) {
	lp, err := r.lookup(loopID)
	if err != nil {
		return nil, err
	}
	spawner := agent.NewSpawner(
		agent.WithTree(lp.tree),
		agent.WithNode(lp.node),
		agent.WithSpawnDepth(lp.node.Depth),
		agent.WithEventStore(lp.store),   // spawn.start/spawn.done land on the same stream
		agent.WithChildBuilder(lp.buildChild), // the binding's per-loop child policy
		agent.WithHumanBus(lp.humanBus),  // bubble a child's undelivered messages up
	)
	h, herr := spawner.Spawn(ctx, agent.SpawnOpts{
		Label:        opts.Label,
		Task:         opts.Task,
		SystemPrompt: opts.SystemPrompt,
		ModelID:      opts.ModelID,
		Tools:        opts.Tools,
		Worker:       opts.Worker,
		Expects:      opts.Expects,
	})
	if herr != nil {
		return nil, herr
	}
	return spawnHandle{h: h}, nil
}

// spawnHandle wraps agent.AgentHandle (ADR-0026) so a binding awaits a child
// without importing internal/agent's async internals at the call site.
type spawnHandle struct{ h agent.AgentHandle }

func (s spawnHandle) NodeID() string       { return s.h.NodeID }
func (s spawnHandle) Wait() agent.SpawnDone { return s.h.Wait() }
func (s spawnHandle) Result() (any, error)  { return s.h.Result() }
