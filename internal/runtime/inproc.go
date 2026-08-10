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

// ErrLoopFinished is returned by Send when the target loop's run goroutine has
// already completed. Its human-bus injector no longer drains at a turn boundary, so
// enqueuing would strand the message forever; Send reports this distinguishable
// condition instead of silently accepting the input.
var ErrLoopFinished = errors.New("runtime: loop finished")

// Deps are the binding-supplied collaborators the Runtime composes. None are
// renderer/TUI/gate types; a binding wires those around the Runtime, not into it.
type Deps struct {
	// BuildAgent is the per-loop construction factory: for one loop it builds the
	// root *agent.Agent AND the loop's child-builder, both bound to THIS loop's own
	// event store and tree (the two per-loop values the Runtime opens/creates at
	// StartLoop). It is the one seam through which a binding injects its engine
	// construction WITHOUT the Runtime importing cmd-layer builders, and the mechanism
	// that de-globalizes the event-store read path (change 0046): the store flows in
	// as a VALUE, so N concurrent loops on one Runtime never share a store.
	//
	// Returns the root agent, the loop's child-builder (agent.ChildBuilder — an agent
	// type, not a renderer/TUI/MCP type, so it does not break the policy-free
	// constraint; may be nil, in which case Spawn runs children with no builder), and
	// the resolved gateway model id. store is the loop's EventStore; tree is the loop's
	// AgentTree (Deps.Tree when the binding observes its own tree, else one the Runtime
	// created); toolReg is the per-loop tool registry.
	BuildAgent func(store event.EventStore, tree *agent.AgentTree, modelID string, toolReg *tools.Registry) (*agent.Agent, agent.ChildBuilder, string, error)
	// NewToolRegistry builds the per-loop tool registry (spawn_agent etc. are wired
	// by the binding via BuildAgent's closure over the same registry). May be nil,
	// in which case an empty registry is used.
	NewToolRegistry func() *tools.Registry
	// BaseDir is where per-loop fsstore event logs live (e.g. session.DefaultLogDir()).
	// When "", StartLoop uses event.NoopStore{} instead of opening an fsstore — the
	// one-shot binding uses this so its behavior stays byte-identical (no new files).
	BaseDir string
	// MaxConcurrent seeds the loop tree's scheduler concurrency (used only when Tree
	// is nil, i.e. when StartLoop creates its own tree).
	MaxConcurrent int
	// Tree, when non-nil, is the agent tree StartLoop drives instead of creating one.
	// The research-probe and shell bindings supply their externally-observed tree so
	// they can still call probe.Summarize / render the tree after the loop runs. When
	// nil, StartLoop creates its own tree from cfg.ModelID + MaxConcurrent.
	Tree *agent.AgentTree
	// DurableStore, when non-nil, is the shared, tenant-scoped, cross-process event
	// store (change 0047). It is threaded in as a VALUE at the composition root
	// (cmd/fuse) — never a construction-time global — preserving the policy-free-seam
	// invariant (ADR-0030) and the de-globalization learning: each StartLoop opens its
	// own StreamKey against this store; a fresh instance derives liveness from the
	// Registry, never from a shared in-memory object. When nil, StartLoop keeps the
	// legacy per-loop fsstore + in-memory-only path so single-loop CLI bindings stay
	// byte-identical (they pass nil).
	DurableStore event.DurableStore
	// Registry, when non-nil, is the durable loop-existence/liveness registry paired
	// with DurableStore (usually the same value). It is the source of truth that lets
	// inProcRuntime.lookup resolve a loop a PRIOR process started even though r.loops
	// is empty on this instance. When nil, resolution is in-memory-only (r.loops).
	Registry event.LoopRegistry
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
	key        event.StreamKey // durable (tenant, loop) identity (change 0047)
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
// deps.BaseDir/<rootID> at StartLoop time. It returns the Runtime interface so a
// binding consumes the seam abstractly (the concrete *inProcRuntime is unexported).
func New(deps Deps) Runtime {
	return &inProcRuntime{deps: deps, loops: map[string]*loop{}}
}

// StartLoop constructs and drives one agent loop from cfg (see the Runtime
// interface). It wraps agent.New (via deps.BuildAgent) + SetEventSink/
// SetNodeIdentity + Agent.Run; the run executes in a goroutine and the returned
// handle awaits it.
func (r *inProcRuntime) StartLoop(ctx context.Context, cfg LoopConfig) (LoopHandle, error) {
	// Use a caller-supplied tree when the binding externally observes it
	// (research-probe/shell keep their tree for probe.Summarize / rendering); else
	// create one from cfg + MaxConcurrent.
	tree := r.deps.Tree
	if tree == nil {
		tree = agent.NewAgentTreeWithConcurrency(cfg.ModelID, cfg.ModelID, r.deps.MaxConcurrent)
	}
	rootID := tree.RootID()
	rootNode := tree.Node(rootID)

	tenant := event.NormalizeTenant(cfg.Tenant)
	key := event.StreamKey{Tenant: tenant, Loop: event.LoopID(rootID)}

	// Store selection (change 0047):
	//   - DurableStore present ⇒ the loop emits into the SHARED, cross-process store
	//     via a per-loop StreamKey adapter (durableSink), and its existence/liveness is
	//     registered in the durable Registry so a cold instance can resolve it.
	//   - else BaseDir == "" ⇒ NoopStore (one-shot keeps byte-identical behavior).
	//   - else the legacy per-loop fsstore under BaseDir/<rootID>.
	var store event.EventStore
	durable := r.deps.DurableStore != nil
	if durable {
		store = &durableSink{store: r.deps.DurableStore, key: key}
	} else if r.deps.BaseDir == "" {
		store = event.NoopStore{}
	} else {
		fs, err := fsstore.NewFSEventStore(r.deps.BaseDir, rootID)
		if err != nil {
			return nil, fmt.Errorf("runtime: open event store: %w", err)
		}
		store = fs
	}

	// Register the loop's existence + mark it live BEFORE the run goroutine starts, so a
	// concurrent cold instance can resolve it (and observe live) immediately. Register is
	// an idempotent upsert that writes Live:true and the owner in ONE row-write — the run
	// goroutine later flips liveness via SetLive(false) at completion. (No redundant
	// SetLive(true) here: Register already recorded liveness + ownership.)
	if r.deps.Registry != nil {
		if err := r.deps.Registry.Register(ctx, event.LoopRecord{Key: key, OwnerNodeID: rootID, Live: true}); err != nil {
			return nil, fmt.Errorf("runtime: register loop: %w", err)
		}
	}

	var toolReg *tools.Registry
	if r.deps.NewToolRegistry != nil {
		toolReg = r.deps.NewToolRegistry()
	} else {
		toolReg = tools.NewRegistry()
	}

	// BuildAgent is the per-loop factory: it binds the root agent AND the loop's
	// child-builder to THIS loop's own store + tree (change 0046 de-globalization).
	// The store flows in as a value, so N concurrent loops never share one.
	a, buildChild, _, err := r.deps.BuildAgent(store, tree, cfg.ModelID, toolReg)
	if err != nil {
		if c, ok := store.(interface{ Close() error }); ok {
			_ = c.Close()
		}
		return nil, fmt.Errorf("runtime: build agent: %w", err)
	}
	a.SetEventSink(store)
	a.SetNodeIdentity(rootNode.ID, rootNode.ParentID, rootNode.Depth)

	lp := &loop{
		id:         rootID,
		key:        key,
		tree:       tree,
		node:       rootNode,
		store:      store,
		humanBus:   agent.NewHumanBus(tree),
		buildChild: buildChild,
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
		// Mark the loop finished in the durable registry so a cold instance resolves it
		// as replay-only (Send ⇒ ErrLoopFinished). Use context.Background(): the run's
		// ctx may already be canceled at completion, but the liveness flip must land.
		if r.deps.Registry != nil {
			_ = r.deps.Registry.SetLive(context.Background(), key, false, rootID)
		}
		// Close the loop's event store on run completion: it releases the fsstore write
		// handle (no per-loop file-handle leak) AND closes its live subscriber channels,
		// so a loop.observe pump terminates without relying on client-ctx or process
		// exit. Attach still works afterward — fsstore.Replay opens its own reader
		// independent of the (now-closed) write handle. NoopStore and the durable-sink
		// adapter have no-op Close (the shared durable store must NOT be closed per loop),
		// so this stays byte-identical for the legacy paths.
		if c, ok := store.(interface{ Close() error }); ok {
			_ = c.Close()
		}
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

// lookup returns the loop for loopID under the mutex, or ErrLoopNotFound. This is
// the LEGACY resolution path (r.loops is the source of truth); Send still uses it
// because injecting a human message requires the live in-process loop object.
func (r *inProcRuntime) lookup(loopID string) (*loop, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	lp, ok := r.loops[loopID]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrLoopNotFound, loopID)
	}
	return lp, nil
}

// loopCache returns the cached live *loop for (tenant, loopID) under the mutex, or
// nil. With the durable seam present, r.loops is demoted to a per-instance CACHE: a
// hit is the live loop; a miss falls through to the durable Registry (see
// resolveDurable). The map key is the BARE loopID, but the durable layer is strictly
// tenant-scoped — so a hit is only valid when the cached loop's tenant matches the
// requested tenant. On a tenant MISMATCH this returns nil (treated as a cache miss),
// forcing the tenant-scoped durable fall-through rather than returning another
// tenant's live store. tenant is normalized so "" and event.DefaultTenant agree.
func (r *inProcRuntime) loopCache(tenant event.TenantID, loopID string) *loop {
	r.mu.Lock()
	defer r.mu.Unlock()
	lp := r.loops[loopID]
	if lp == nil {
		return nil
	}
	if lp.key.Tenant != event.NormalizeTenant(tenant) {
		return nil // tenant mismatch: fall through to the tenant-scoped durable registry
	}
	return lp
}

// resolveDurable resolves a (tenant, loopID) against the durable Registry when the
// r.loops cache misses. It maps event.ErrLoopUnknown to runtime.ErrLoopNotFound so
// callers see one "not found" sentinel regardless of backend. It returns
// (record, true, nil) when the loop exists durably.
func (r *inProcRuntime) resolveDurable(ctx context.Context, tenant event.TenantID, loopID string) (event.LoopRecord, event.StreamKey, error) {
	key := event.StreamKey{Tenant: event.NormalizeTenant(tenant), Loop: event.LoopID(loopID)}
	if r.deps.Registry == nil {
		return event.LoopRecord{}, key, fmt.Errorf("%w: %q", ErrLoopNotFound, loopID)
	}
	rec, err := r.deps.Registry.Resolve(ctx, key)
	if err != nil {
		if errors.Is(err, event.ErrLoopUnknown) {
			return event.LoopRecord{}, key, fmt.Errorf("%w: %q", ErrLoopNotFound, loopID)
		}
		return event.LoopRecord{}, key, err
	}
	return rec, key, nil
}

// Observe returns a live event tail plus an idempotent unsubscribe. Delivery is
// non-blocking end to end (ADR-0016/0025): a slow observer drops the newest event
// with a gap rather than wedging the loop. A cache hit subscribes to the loop's own
// store; a cache miss + durable Resolve subscribes to the shared durable store's
// cross-instance channel (so a loop a prior process started is observable here).
func (r *inProcRuntime) Observe(ctx context.Context, tenant event.TenantID, loopID string) (<-chan event.Event, func(), error) {
	if lp := r.loopCache(tenant, loopID); lp != nil {
		ch, cancel := lp.store.Subscribe()
		return ch, cancel, nil
	}
	_, key, err := r.resolveDurable(ctx, tenant, loopID)
	if err != nil {
		return nil, nil, err
	}
	return r.deps.DurableStore.Subscribe(ctx, key)
}

// Attach returns durable history with Seq > from for reconnect. A cache hit replays
// the loop's own store; a cache miss + durable Resolve replays the shared durable
// store (the cold cross-process reattach path — the 0047 fix).
func (r *inProcRuntime) Attach(ctx context.Context, tenant event.TenantID, loopID string, from event.Seq) ([]event.Event, error) {
	if lp := r.loopCache(tenant, loopID); lp != nil {
		return lp.store.Replay(from)
	}
	_, key, err := r.resolveDurable(ctx, tenant, loopID)
	if err != nil {
		return nil, err
	}
	return r.deps.DurableStore.Replay(ctx, key, from)
}

// Send injects human input at the loop's next turn boundary (ADR-0022 human-bus).
// It enqueues for the root node; the root injector installed at StartLoop drains it
// at the next turn boundary. ModeRespond = a direct reply to that node (not a
// broadcast).
//
// Send distinguishes three cases: an unknown loop returns ErrLoopNotFound; a loop
// whose run goroutine has already finished returns ErrLoopFinished (its injector no
// longer drains, so enqueuing would strand the message forever); a still-running
// loop enqueues and returns nil.
func (r *inProcRuntime) Send(ctx context.Context, tenant event.TenantID, loopID string, input string) error {
	lp := r.loopCache(tenant, loopID)
	if lp == nil {
		// Not live in THIS instance's cache (or a tenant mismatch on the bare-loopID
		// cache key — loopCache returns nil for a wrong-tenant hit). With the durable seam
		// present, resolve the loop's existence/liveness UNDER THE REQUESTED TENANT: a
		// finished loop ⇒ ErrLoopFinished (replay-only); a still-live loop owned by ANOTHER
		// instance ⇒ ErrLoopNotFound here (this instance holds no injector for it); an
		// unknown loop (or one under a different tenant) ⇒ ErrLoopNotFound. Without the
		// seam, resolveDurable returns ErrLoopNotFound directly (legacy in-memory behavior).
		rec, _, rerr := r.resolveDurable(ctx, tenant, loopID)
		if rerr != nil {
			return rerr
		}
		if !rec.Live {
			return fmt.Errorf("%w: %q", ErrLoopFinished, loopID)
		}
		return fmt.Errorf("%w: %q", ErrLoopNotFound, loopID)
	}
	// A finished loop's injector will never drain another message. Report it instead
	// of silently stranding the input on the queue.
	if lp.handle != nil {
		select {
		case <-lp.handle.done:
			return fmt.Errorf("%w: %q", ErrLoopFinished, loopID)
		default:
		}
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
		agent.WithEventStore(lp.store),        // spawn.start/spawn.done land on the same stream
		agent.WithChildBuilder(lp.buildChild), // the binding's per-loop child policy
		agent.WithHumanBus(lp.humanBus),       // bubble a child's undelivered messages up
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

// durableSink adapts an event.DurableStore + fixed StreamKey down to the legacy
// event.EventStore interface (Append(Event)/Subscribe()/Replay(Seq)) that the agent
// engine and Spawner consume as their event sink. It lets a loop emit into the
// shared, cross-process durable store (change 0047) without the engine learning the
// tenant/context/key vocabulary. It carries a background context for the store ops:
// the loop's own run ctx governs the RUN, but event emission must not be canceled by
// a client disconnect (mirrors the completion SetLive using context.Background()).
//
// Close is a NO-OP: the durable store is SHARED across every loop and instance, so a
// single loop's completion must never close it (that would tear down other loops'
// streams and the cross-instance tail). This is the counterpart to the legacy
// fsstore's per-loop Close — durability + subscriber lifetime are the store's own.
type durableSink struct {
	store event.DurableStore
	key   event.StreamKey
}

func (d *durableSink) Append(e event.Event) error {
	return d.store.Append(context.Background(), d.key, e)
}

func (d *durableSink) Subscribe() (<-chan event.Event, func()) {
	ch, cancel, err := d.store.Subscribe(context.Background(), d.key)
	if err != nil {
		// Degrade gracefully to a closed channel (a consumer range ends at once) rather
		// than nil-panicking — mirrors NoopStore.Subscribe's contract.
		closed := make(chan event.Event)
		close(closed)
		return closed, func() {}
	}
	return ch, cancel
}

func (d *durableSink) Replay(from event.Seq) ([]event.Event, error) {
	return d.store.Replay(context.Background(), d.key, from)
}

// Close is intentionally a no-op (see the durableSink doc): the shared durable store
// must not be closed on a single loop's completion.
func (d *durableSink) Close() error { return nil }

// spawnHandle wraps agent.AgentHandle (ADR-0026) so a binding awaits a child
// without importing internal/agent's async internals at the call site.
type spawnHandle struct{ h agent.AgentHandle }

func (s spawnHandle) NodeID() string        { return s.h.NodeID }
func (s spawnHandle) Wait() agent.SpawnDone { return s.h.Wait() }
func (s spawnHandle) Result() (any, error)  { return s.h.Result() }
