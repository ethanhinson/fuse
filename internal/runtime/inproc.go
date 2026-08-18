package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/event/fsstore"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/observe"
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
	// Observer receives timing-only lifecycle signals. Nil is identical to a no-op.
	Observer observe.Observer
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
	// LeaseTTL is the owner-liveness lease duration for a live loop (change 0049 Task 7).
	// While a loop runs, a background renewer Heartbeats the registry every ~⅓ TTL to
	// push LeaseExpiry to now+TTL, so a cold instance that resolves the loop can tell a
	// live owner from an abandoned one (an expired lease ⇒ re-ownable). Zero ⇒ a sane
	// default (defaultLeaseTTL). Ignored when Registry is nil (in-memory-only bindings).
	LeaseTTL time.Duration
	// HeartbeatInterval, when > 0, overrides the renewer's tick cadence (default ~⅓
	// LeaseTTL). It exists so tests can drive the renewer fast without a fake-clock
	// library; production leaves it zero.
	HeartbeatInterval time.Duration
	// Now, when non-nil, is the clock seam used for lease math (LeaseExpiry = Now()+TTL
	// on Register/Heartbeat, and the reap comparison Now().After(rec.LeaseExpiry)). It
	// defaults to time.Now. Tests inject a frozen clock to make the reap/re-own decision
	// deterministic. The renewer's tick cadence uses a real timer regardless — Now only
	// governs the expiry timestamps and the reap comparison.
	Now func() time.Time
	// LoopTeardown, when non-nil, is invoked once at loop-run completion with the
	// loop's OWN tool registry (the one NewToolRegistry produced and BuildAgent wired).
	// It is the per-loop cleanup seam a binding uses to release resources it attached
	// to that registry — notably the loop-server's per-loop mcp.Manager (change #59):
	// the binding closes the manager it constructed for this registry, so N concurrent
	// loops each tear down their OWN manager and none leaks past its loop. It runs
	// alongside the loop's event-store Close, on the run goroutine, after the run
	// returns. It takes a *tools.Registry (already in the runtime's import set) — never
	// an mcp/auth type — so the policy-free seam is preserved (ADR-0030). Nil ⇒ no-op.
	LoopTeardown func(toolReg *tools.Registry)
	// LoopContext, when non-nil, decorates the loop-lifetime context the loop's run
	// (and thus every tool Execute) derives from, once per StartLoop. It is the
	// policy-free seam the composition root (cmd/fuse) uses to stamp the authenticated
	// loop-initiator's identity onto the run context — via toolidentity.WithPrincipal —
	// so the MCP egress mints a per-call, per-principal delegation token (change #59).
	//
	// The runtime NEVER learns loopauth/toolidentity: it only invokes this closure with
	// the (context, LoopConfig) it already holds, so the auth import graph stays out of
	// internal/runtime (ADR-0030). The decorated context is per-loop, so two concurrent
	// loops started by different principals derive independent identities and never
	// cross. Nil ⇒ the run context is used verbatim (byte-identical to pre-#59).
	LoopContext func(ctx context.Context, cfg LoopConfig) context.Context

	// IdleTTL bounds an interactive (persistent conversational) session's lifetime
	// after its connection drops (change 0054, D2). An interactive loop runs under a
	// session-scoped context DETACHED from the request ctx, so a client disconnect no
	// longer cancels the parked run — but the session must not live forever. When a
	// loop goes IdleTTL with no Send and no live Observe, the reaper cancels its
	// session ctx, the run finishes, and the registry flips to not-live. Each Send and
	// each Observe "touches" the loop, resetting the idle timer. Zero ⇒ defaultIdleTTL.
	// Ignored for non-interactive loops (they own their run to completion and are not
	// connection-pinned in a way this changes).
	IdleTTL time.Duration
}

// defaultLeaseTTL is the owner-liveness lease duration used when Deps.LeaseTTL is zero.
const defaultLeaseTTL = 30 * time.Second

// defaultIdleTTL bounds an interactive session's post-disconnect lifetime when
// Deps.IdleTTL is zero (change 0054, D2). A single named constant for now; a
// per-tenant / configurable policy is a follow-up (spec Out of scope).
const defaultIdleTTL = 30 * time.Minute

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

	// turns owns the loop's turn-scoped span topology (change 0071). It is held on the
	// loop — not just in launchLoop's frame — because it must survive every park and
	// outlive the request that started the session. Nil for a one-shot loop, which has
	// no turn boundaries.
	turns *turnTracer

	// touch resets the interactive session's idle-reap timer (change 0054, D2). It is
	// called on every Send and every Observe so an actively-used session is never
	// reaped; nil for a non-interactive loop (which has no reaper). Safe to call
	// concurrently.
	touch func()
}

// New builds an in-process Runtime. deps carries the collaborators a binding
// constructs (model Completer/agent factory, tool registry factory, config) so the
// Runtime stays free of cmd-layer policy. The event store is opened per-loop under
// deps.BaseDir/<rootID> at StartLoop time. It returns the Runtime interface so a
// binding consumes the seam abstractly (the concrete *inProcRuntime is unexported).
func New(deps Deps) Runtime {
	if deps.Observer == nil {
		deps.Observer = observe.NoopObserver{}
	}
	return &inProcRuntime{deps: deps, loops: map[string]*loop{}}
}

// now returns the runtime's clock: the injected Deps.Now seam when set, else time.Now.
// It governs lease-expiry timestamps and the reap comparison so tests can freeze time.
func (r *inProcRuntime) now() time.Time {
	if r.deps.Now != nil {
		return r.deps.Now()
	}
	return time.Now()
}

// leaseTTL returns the configured owner-liveness lease duration, or defaultLeaseTTL
// when unset (or non-positive).
func (r *inProcRuntime) leaseTTL() time.Duration {
	if r.deps.LeaseTTL > 0 {
		return r.deps.LeaseTTL
	}
	return defaultLeaseTTL
}

// idleTTL returns the interactive-session idle timeout, or defaultIdleTTL when unset
// (or non-positive). See Deps.IdleTTL.
func (r *inProcRuntime) idleTTL() time.Duration {
	if r.deps.IdleTTL > 0 {
		return r.deps.IdleTTL
	}
	return defaultIdleTTL
}

// heartbeatInterval returns the renewer's tick cadence: the explicit override when set,
// else ~⅓ of the lease TTL (with a small floor so a tiny TTL never yields a zero tick).
func (r *inProcRuntime) heartbeatInterval() time.Duration {
	if r.deps.HeartbeatInterval > 0 {
		return r.deps.HeartbeatInterval
	}
	iv := r.leaseTTL() / 3
	if iv <= 0 {
		iv = time.Millisecond
	}
	return iv
}

// startLeaseRenewer launches the background heartbeat renewer for a live loop. It runs
// until ctx is done (the loop's run ctx) OR stop is closed (the run goroutine closes it
// at completion, alongside SetLive(false)), whichever comes first — so the goroutine
// never leaks. Each tick Heartbeats the registry to push LeaseExpiry to now()+TTL under
// this instance's node id (ownerNodeID). Caller guarantees r.deps.Registry != nil.
func (r *inProcRuntime) startLeaseRenewer(ctx context.Context, key event.StreamKey, ownerNodeID string, stop <-chan struct{}) {
	ttl := r.leaseTTL()
	interval := r.heartbeatInterval()
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stop:
				return
			case <-t.C:
				// Heartbeat is best-effort: a transient registry error must not kill the
				// renewer (the next tick retries) — but the loop itself keeps running, so a
				// persistently-failing registry only widens the reap window, it does not stall
				// the loop. Use a background context: a client disconnect must not cancel the
				// liveness renewal (mirrors durableSink / the completion SetLive).
				_ = r.deps.Registry.Heartbeat(context.Background(), key, ownerNodeID, r.now().Add(ttl))
			}
		}
	}()
}

// startIdleReaper launches the per-loop idle-TTL reaper for an interactive session
// (change 0054, D2). It cancels sessionCancel — ending the parked run — once idleTTL
// elapses with no touch (no Send, no live Observe). It returns a touch func the
// caller stores on the loop; each Send/Observe calls it to reset the window. The
// reaper exits (without reaping) when stop is closed (run completion) or sessionCtx
// is done (already ended some other way), so it never leaks and never races a
// completed run. Touches after teardown are harmless no-ops.
func (r *inProcRuntime) startIdleReaper(sessionCtx context.Context, sessionCancel context.CancelFunc, stop <-chan struct{}) func() {
	ttl := r.idleTTL()
	timer := time.NewTimer(ttl)
	touches := make(chan struct{}, 1)
	go func() {
		defer timer.Stop()
		for {
			select {
			case <-sessionCtx.Done():
				return
			case <-stop:
				return
			case <-touches:
				// Reset the idle window on activity. Drain-then-reset is safe: only this
				// goroutine reads the timer channel.
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(ttl)
			case <-timer.C:
				// Idle window elapsed with no activity: reap. Cancelling the session ctx
				// unwinds the parked run, which flips the registry to not-live and tears
				// down the store on the completion goroutine (below).
				sessionCancel()
				return
			}
		}
	}()
	return func() {
		// Non-blocking: a full buffer already has a pending touch queued, which is
		// enough to reset the window on the next loop iteration.
		select {
		case touches <- struct{}{}:
		default:
		}
	}
}

// endOnce wraps a span handle in an end function that forwards AT MOST ONE End to
// the handle, safely under concurrent callers (change 0071 Task 1). The loop-span
// end path used to be guarded by a plain bool, which was sound only while a single
// goroutine could ever reach it; turn-scoped tracing adds a park/teardown caller
// that can race the run goroutine's completion, so the guard is a sync.Once.
//
// FIRST-WRITER-WINS: the first outcome handed to end is the one that lands on the
// span, and every later call is dropped — outcome included. That is deliberate, not
// incidental: the park path ends fuse.loop.run with OutcomeSuccess at the first turn
// boundary, and a later session cancellation (idle reap, disconnect, teardown) must
// NOT rewrite that already-exported span as canceled.
func endOnce(h observe.Handle) func(observe.Outcome) {
	var once sync.Once
	return func(out observe.Outcome) {
		once.Do(func() { h.End(out) })
	}
}

// launchOpts carries the few things that differ between a fresh StartLoop and a
// Resume (change 0054, D4); everything else in the loop-launch is identical, so both
// go through launchLoop.
type launchOpts struct {
	// seed is the initial transcript handed to Agent.Run: {user, task} for a fresh
	// start, or the reconstructed conversation for a resume.
	seed []model.Message
	// seeded marks the seed as a durable-stream reconstruction, so Run suppresses the
	// KindUserInput re-emit for the seed's user turns (they already exist). False for a
	// fresh start (the initial task must emit).
	seeded bool
	// rootID pins the tree's root id — empty for a fresh start (mint one), the original
	// loop_id for a resume (identity must survive the re-park).
	rootID string
	// resume marks a revival of an existing registry record: the loop is re-marked live
	// via SetLive rather than Register'd afresh.
	resume bool
	// carrier is the ORIGINAL session's durable trace context, recovered from the
	// replayed stream (change 0071, spec D2). A resumed loop links its turn roots to
	// it instead of minting a second session root. Nil for a fresh start (which links
	// to its own fuse.loop.run instead).
	carrier *event.TraceCarrier
	// turnIndex is the number of turns the loop already COMPLETED before this launch,
	// used to continue fuse.turn.index across a resume rather than restarting it and
	// colliding with the pre-reap turns of the same loop_id. Zero for a fresh start.
	turnIndex int
}

// StartLoop constructs and drives one agent loop from cfg (see the Runtime
// interface). It wraps agent.New (via deps.BuildAgent) + SetEventSink/
// SetNodeIdentity + Agent.Run; the run executes in a goroutine and the returned
// handle awaits it.
func (r *inProcRuntime) StartLoop(ctx context.Context, cfg LoopConfig) (LoopHandle, error) {
	return r.launchLoop(ctx, cfg, launchOpts{seed: []model.Message{{Role: "user", Content: cfg.Task}}})
}

// launchLoop is the shared construction + run-goroutine wiring behind StartLoop and
// Resume. opts is the small delta between them (see launchOpts).
func (r *inProcRuntime) launchLoop(ctx context.Context, cfg LoopConfig, opts launchOpts) (LoopHandle, error) {
	// A RESUME does NOT start a second fuse.loop.run (change 0071, spec D2): the
	// session root already exists — and, since it ends at the first park, has already
	// been EXPORTED — in the trace of the process that first ran this loop_id. Minting
	// another would give one conversation two competing roots. The revived session's
	// work is covered by turn roots instead, linked back to the restored carrier. A
	// fresh start (one-shot or interactive) is byte-identical to before.
	loopCtx := ctx
	var loopSpan observe.Handle = observe.NoopHandle{}
	if !opts.resume {
		loopCtx, loopSpan = r.deps.Observer.Start(ctx, observe.Descriptor{Kind: observe.OperationLoop, Name: "run", Fields: []observe.Field{{Key: "tenant", Value: string(event.NormalizeTenant(cfg.Tenant))}}})
	}
	end := endOnce(loopSpan)

	// Session lifetime (change 0054, D2). A one-shot loop's run is governed by the
	// request ctx (loopCtx) — byte-identical to before. An INTERACTIVE loop instead
	// runs under a SESSION context detached from the request ctx: WithoutCancel keeps
	// loopCtx's values (the durable trace carrier, and the LoopContext identity applied
	// below) but drops its cancellation, so a client disconnect no longer unwinds the
	// parked run. The session's own cancel is held by the run-completion path and the
	// idle-TTL reaper (started after the loop is registered), which is the ONLY thing
	// that ends a disconnected interactive session. sessionCtx also becomes the
	// event-emission ctx (durableSink), so Appends survive the disconnect too.
	// A resumed loop is always interactive (it re-parks to await the next Send), so the
	// session-detach and reaper apply to it exactly as to a fresh interactive start.
	interactive := cfg.Interactive || opts.resume
	sessionCtx := loopCtx
	var sessionCancel context.CancelFunc
	if interactive {
		sessionCtx, sessionCancel = context.WithCancel(context.WithoutCancel(loopCtx))
	}
	// Use a caller-supplied tree when the binding externally observes it
	// (research-probe/shell keep their tree for probe.Summarize / rendering); else
	// create one from cfg + MaxConcurrent.
	tree := r.deps.Tree
	if tree == nil {
		// opts.rootID pins the id on a resume so the re-parked loop keeps its original
		// loop_id (empty ⇒ a fresh id for a normal start).
		tree = agent.NewAgentTreeWithRootID(cfg.ModelID, cfg.ModelID, r.deps.MaxConcurrent, opts.rootID)
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
		store = &durableSink{store: r.deps.DurableStore, key: key, ctx: sessionCtx, observer: r.deps.Observer}
	} else if r.deps.BaseDir == "" {
		store = event.NoopStore{}
	} else {
		fs, err := fsstore.NewFSEventStore(r.deps.BaseDir, rootID)
		if err != nil {
			end(observe.OutcomeError)
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
		// Seed the initial owner-liveness lease at Register time (change 0049 Task 7): the
		// renewer below advances it every ~⅓ TTL, and a cold instance treats an expired
		// lease on a still-live record as abandoned (see resolveDurable's reap/re-own).
		leaseExpiry := r.now().Add(r.leaseTTL())
		if opts.resume {
			// The record already exists (this is a revival of a finished/evicted loop,
			// change 0054 D4): re-mark it live under this instance and refresh the lease,
			// rather than Register'ing a duplicate. Heartbeat sets both LeaseExpiry and
			// owner; SetLive flips Live back to true.
			if err := r.deps.Registry.Heartbeat(ctx, key, rootID, leaseExpiry); err != nil {
				end(observe.OutcomeError)
				return nil, fmt.Errorf("runtime: resume heartbeat: %w", err)
			}
			if err := r.deps.Registry.SetLive(ctx, key, true, rootID); err != nil {
				end(observe.OutcomeError)
				return nil, fmt.Errorf("runtime: resume set-live: %w", err)
			}
		} else if err := r.deps.Registry.Register(ctx, event.LoopRecord{Key: key, OwnerNodeID: rootID, Live: true, LeaseExpiry: leaseExpiry}); err != nil {
			end(observe.OutcomeError)
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
		// Symmetric teardown (change #59): NewToolRegistry may already have attached
		// live resources to toolReg — notably the loop-server's per-loop mcp.Manager
		// (dialed servers + read-pump/notify goroutines). A StartLoop failure after that
		// point must release them, or the manager and its goroutines leak and the
		// binding's per-loop tracking map is never cleared. The completion goroutine
		// below is never reached on this early-return path, so run it here.
		if r.deps.LoopTeardown != nil {
			r.deps.LoopTeardown(toolReg)
		}
		end(observe.OutcomeError)
		return nil, fmt.Errorf("runtime: build agent: %w", err)
	}
	// Turn-scoped tracing (change 0071), INTERACTIVE ONLY: a one-shot loop never builds
	// a turnTracer, keeps Deps.Observer verbatim, and therefore emits exactly the span
	// shape it emitted before this change. Constructed here, after the last early
	// return in this function (learnings:
	// per-instance-resource-needs-teardown-on-every-early-return) — everything below is
	// unconditional, so the run goroutine's completion path is the only teardown site
	// the tracer needs. Keep it that way: a new early return added after this point
	// MUST call turns.teardown before returning.
	var turns *turnTracer
	obs := r.deps.Observer
	if interactive {
		// The link source for every turn root: this launch's own session root for a fresh
		// interactive start, or the ORIGINAL session's restored context for a resume.
		// Taken through the capability helper — never a type assertion to the otel
		// observer (ADR-0040) — so a provider without propagation simply yields nil and
		// turn roots are unlinked rather than absent. Read here, once, so it survives
		// every park: the loop context it comes from outlives the request.
		traceLink := opts.carrier
		if traceLink == nil {
			traceLink = observe.TraceCarrier(r.deps.Observer, loopCtx)
		}
		turns = newTurnTracer(r.deps.Observer, sessionCtx, traceLink,
			[]observe.Field{{Key: "loop_id", Value: rootID}, {Key: "tenant", Value: string(tenant)}},
			opts.turnIndex)
		// The agent observes through the turn-scoped wrapper so its per-turn work nests
		// under the open turn root instead of the long-ended session root.
		obs = turnScopedObserver{inner: r.deps.Observer, turns: turns}
		if opts.resume {
			// A resume IS a turn boundary: the revived loop re-runs its transcript and
			// answers immediately, before any park/wake pair can fire. Opening the turn
			// here (rather than at the first wake) is also what gives Agent.Run an active
			// durable carrier to stamp on the resumed session's events.
			turns.wake()
		}
	}
	a.SetObserver(obs)
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
		turns:      turns,
	}
	r.mu.Lock()
	if r.loops == nil {
		r.loops = map[string]*loop{}
	}
	r.loops[rootID] = lp
	r.mu.Unlock()

	// Root human-message injector so Send() lands at a turn boundary (ADR-0022).
	injector := agent.NewHumanInjector(rootNode.ID, lp.humanBus)
	if turns != nil {
		// The injector's park/wake is the ONE authoritative turn boundary (spec D3):
		// entering the wait is the end of an exchange, waking on a human message is the
		// start of the next. Both callbacks run synchronously on the run goroutine, so
		// they only start/end a span and never block. The park callback closes over
		// end, which is endOnce-guarded — the first park ends fuse.loop.run with
		// success, and a later reap or cancellation cannot rewrite that.
		injector.SetTurnBoundary(func() { turns.park(end) }, turns.wake)
	}
	a.SetHumanInjector(injector)

	// Persistent conversational mode (opt-in): the loop parks at each terminal turn
	// boundary awaiting the next Send, so one loop_id carries a multi-turn chat with
	// server-authoritative history. Only meaningful alongside the injector above. An
	// interactive loop must run uncapped: each resumed exchange consumes real turns,
	// so any finite per-run backstop the binding baked into the agent would end the
	// whole conversation once total turns crossed it. Lift it here.
	if interactive {
		a.SetInteractive(true)
		a.SetMaxTurns(0)
	}
	// A resumed seed IS a durable-stream reconstruction, so suppress re-emitting
	// KindUserInput for its user turns (change 0054): they already exist in the stream.
	if opts.seeded {
		a.SetSeeded(true)
	}

	h := &loopHandle{id: rootID, done: make(chan struct{})}
	lp.handle = h

	// Owner-liveness lease renewer (change 0049 Task 7): while the loop runs, refresh
	// LeaseExpiry every ~⅓ TTL so a cold instance can tell a live owner from an
	// abandoned one. stopRenew is closed at run completion (below) so the renewer exits
	// promptly even if ctx is not canceled; the renewer ALSO exits on ctx.Done, so it
	// never leaks. Only when a durable Registry is present — nil-registry single-loop
	// bindings run no renewer.
	// The renewer follows the loop's OWN lifetime, not the request's: for an
	// interactive loop it must keep heartbeating after the client disconnects (the
	// session survives), so it is driven by sessionCtx (== loopCtx for a one-shot).
	var stopRenew chan struct{}
	if r.deps.Registry != nil {
		stopRenew = make(chan struct{})
		r.startLeaseRenewer(sessionCtx, key, rootID, stopRenew)
	}

	// Idle-TTL reaper for an interactive session (change 0054, D2): once the loop is
	// registered and live, arm the reaper so a disconnected-and-idle session is bounded.
	// stopReap is closed at run completion so it exits promptly; it also exits on
	// sessionCtx.Done, so it never leaks. touch is stored on the loop for Send/Observe.
	var stopReap chan struct{}
	if interactive && sessionCancel != nil {
		stopReap = make(chan struct{})
		lp.touch = r.startIdleReaper(sessionCtx, sessionCancel, stopReap)
	}

	// Decorate the run context with the composition root's per-loop identity hook
	// (change #59). The runtime never learns what the decorator stamps — it only
	// invokes the closure — so the auth import graph stays out of internal/runtime
	// (ADR-0030). This is the seam where the authenticated loop-initiator's Principal
	// reaches every tool Execute (via toolidentity.WithPrincipal, supplied by cmd/fuse),
	// so the MCP egress mints per-principal, per-tenant tokens. Per-loop and applied
	// once here, so it holds for every turn without per-turn re-plumbing; two concurrent
	// loops derive independent contexts and never cross. It decorates sessionCtx (the
	// disconnect-detached run ctx for interactive loops; == loopCtx for one-shots), so
	// identity survives a resume/re-park unchanged.
	runCtx := sessionCtx
	if r.deps.LoopContext != nil {
		runCtx = r.deps.LoopContext(sessionCtx, cfg)
	}

	go func() {
		defer close(h.done)
		msgs, rerr := a.Run(runCtx, opts.seed)
		out := observe.OutcomeSuccess
		if errors.Is(rerr, context.DeadlineExceeded) {
			out = observe.OutcomeTimeout
		} else if errors.Is(rerr, context.Canceled) {
			out = observe.OutcomeCanceled
		} else if rerr != nil {
			out = observe.OutcomeError
		}
		// Defensive span teardown (change 0071), symmetric with the store Close and
		// LoopTeardown below: a session that dies mid-exchange (idle reap, run error,
		// shutdown) still holds an OPEN turn root, and an un-ended span is never
		// exported at all. End it with the run's own terminal outcome, the same
		// derivation fuse.loop.run uses, before ending the session root. Guarded rather
		// than relying on the nil-receiver no-op, so a one-shot run demonstrably takes
		// the same path it always did.
		if turns != nil {
			turns.teardown(out)
		}
		end(out)
		h.msgs, h.err = msgs, rerr
		// Stop the lease renewer FIRST so it can no longer race a Heartbeat past the
		// SetLive(false) below (a stray post-completion Heartbeat would re-mark a stale
		// lease as fresh). The renewer also observes ctx.Done, but closing stopRenew makes
		// the shutdown deterministic and immediate.
		if stopRenew != nil {
			close(stopRenew)
		}
		// Stop the idle reaper too, and release the session context (change 0054): the
		// run is over, so the reaper must not fire, and the WithoutCancel-derived session
		// ctx should be canceled to free its resources. sessionCancel is idempotent, so
		// a reaper-initiated completion (which already called it) is unaffected.
		if stopReap != nil {
			close(stopReap)
		}
		if sessionCancel != nil {
			sessionCancel()
		}
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
		// Per-loop resource teardown (change #59): release anything the binding
		// attached to THIS loop's registry — the loop-server's per-loop mcp.Manager —
		// so no manager (and its read-pump/notify goroutines) outlives its loop.
		if r.deps.LoopTeardown != nil {
			r.deps.LoopTeardown(toolReg)
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
	// Reap / re-own on resolve (change 0049 Task 7): a record that is still marked Live
	// but whose lease has EXPIRED describes a loop whose owning instance died without
	// flipping SetLive(false) — an abandoned live loop. Rather than surface it as a hard
	// error or refuse observe/replay, the cold-resolving instance re-owns it: refresh the
	// lease under THIS node id (Heartbeat with now()+TTL) and record the refreshed record
	// so observe/attach/replay of the abandoned loop proceeds. This does NOT resurrect a
	// one-shot's execution nor resume a parked persistent loop's driving (that rides #53);
	// it only clears the abandoned-lease as a blocker to reading the stream. A non-expired
	// live record is left to its owner untouched; a not-live (completed) record is served
	// replay unchanged (its lease is irrelevant once it is done).
	if rec.Live && !rec.LeaseExpiry.IsZero() && r.now().After(rec.LeaseExpiry) {
		newExpiry := r.now().Add(r.leaseTTL())
		// The re-owning node id is the loop's own id (rootID == loopID == key.Loop, the same
		// value StartLoop records as OwnerNodeID) — this instance now serves the abandoned
		// loop's stream. Best-effort: on Heartbeat failure fall back to the record as
		// resolved (the caller still gets a usable, non-error result — the lease simply stays
		// stale until the next resolve retries).
		reOwner := string(key.Loop)
		if err := r.deps.Registry.Heartbeat(ctx, key, reOwner, newExpiry); err == nil {
			rec.LeaseExpiry = newExpiry
			rec.OwnerNodeID = reOwner
		}
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
		// A live Observe is session activity: reset the idle-reap window (change 0054,
		// D2) so a loop a client is actively tailing is never reaped out from under it.
		if lp.touch != nil {
			lp.touch()
		}
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

// Resume revives a persistent conversational session (change 0054, D4). See the
// Runtime interface for the contract. Three cases:
//
//  1. Live in THIS instance's cache (tenant-matched) → no-op: the loop is still
//     parked, so resume is just "re-open your Observe". Return the live handle.
//  2. Resolvable in the durable registry and NOT live (finished / idle-reaped /
//     completed on a prior process) → rehydrate: replay the durable stream, fold it
//     to the model-facing transcript, and re-launch a re-parked loop under the SAME
//     loop_id seeded with that transcript (launchLoop with resume opts).
//  3. Resolvable and live but owned by another instance → not resumable here (this
//     instance holds no injector for it); report ErrLoopNotFound so the caller does
//     not double-drive a loop another instance owns.
//
// Requires the durable Registry + DurableStore; without them there is no cross-
// process identity to resume (a nil-registry single-loop binding never evicts a loop
// it still holds, so case 1 covers it, and an unknown loop is ErrLoopNotFound).
func (r *inProcRuntime) Resume(ctx context.Context, tenant event.TenantID, loopID string) (LoopHandle, error) {
	// Case 1: still live locally under the requested tenant.
	if lp := r.loopCache(tenant, loopID); lp != nil {
		if lp.touch != nil {
			lp.touch()
		}
		return lp.handle, nil
	}

	// Resolve tenant-scoped (ADR-0034): a cross-tenant or unknown loop is ErrLoopNotFound,
	// never a leak (resolveDurable maps ErrLoopUnknown → ErrLoopNotFound).
	rec, key, err := r.resolveDurable(ctx, tenant, loopID)
	if err != nil {
		return nil, err
	}
	if r.deps.DurableStore == nil {
		// No shared store to replay from — nothing to rehydrate. (Only reachable with a
		// Registry but no DurableStore, an unusual binding.)
		return nil, fmt.Errorf("%w: %q (no durable store to resume from)", ErrLoopNotFound, loopID)
	}

	// Case 3: live but owned elsewhere (resolveDurable already re-owned an EXPIRED lease;
	// a still-fresh live lease means another instance is actively driving it). This
	// instance cannot inject into a loop it does not hold.
	if rec.Live && !rec.LeaseExpiry.IsZero() && !r.now().After(rec.LeaseExpiry) {
		return nil, fmt.Errorf("%w: %q (live on another instance)", ErrLoopNotFound, loopID)
	}

	// Case 2: rehydrate. Replay the durable stream from the beginning and fold it back
	// into the model-facing transcript (D1/D5), then re-launch a re-parked loop under the
	// same loop_id seeded with that transcript.
	events, err := r.deps.DurableStore.Replay(ctx, key, 0)
	if err != nil {
		return nil, fmt.Errorf("runtime: resume replay: %w", err)
	}
	seed, err := reconstructMessages(events)
	if err != nil {
		return nil, fmt.Errorf("runtime: resume reconstruct: %w", err)
	}

	cfg := LoopConfig{
		ModelID:     modelFromEvents(events),
		Tenant:      event.NormalizeTenant(tenant),
		Interactive: true,
	}
	// The replayed stream is also where the resumed session's TRACE identity comes from
	// (change 0071): every event carries the durable carrier of the session that emitted
	// it, and every completed exchange left exactly one loop.parked behind — so the
	// stream supplies both the link target for the resumed turn roots and the turn index
	// to continue from.
	return r.launchLoop(ctx, cfg, launchOpts{
		seed:      seed,
		seeded:    true,
		rootID:    loopID,
		resume:    true,
		carrier:   carrierFromEvents(events),
		turnIndex: completedTurns(events),
	})
}

// modelFromEvents recovers the gateway model id a loop ran under from its durable
// stream (the model.call.start payload carries it), so a resumed loop re-parks under
// the same model. Falls back to "" (the binding resolves a default) when absent.
func modelFromEvents(events []event.Event) string {
	for _, e := range events {
		if e.Kind == event.KindModelCallStart {
			var p event.ModelCallStartPayload
			if err := json.Unmarshal(e.Payload, &p); err == nil && p.Model != "" {
				return p.Model
			}
		}
	}
	return ""
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
	// A Send is session activity: reset the idle-reap window (change 0054, D2) before
	// enqueuing, so an actively-driven conversation is never reaped mid-exchange.
	if lp.touch != nil {
		lp.touch()
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
		agent.WithObserver(r.deps.Observer),
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
	store    event.DurableStore
	key      event.StreamKey
	ctx      context.Context
	observer observe.Observer
}

func (d *durableSink) Append(e event.Event) error {
	ctx, span := d.start(observe.OperationStore, "append")
	err := d.store.Append(ctx, d.key, e)
	span.End(observeOutcome(err))
	return err
}

func (d *durableSink) Subscribe() (<-chan event.Event, func()) {
	ctx, span := d.start(observe.OperationPubSub, "subscribe")
	ch, cancel, err := d.store.Subscribe(ctx, d.key)
	span.End(observeOutcome(err))
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
	ctx, span := d.start(observe.OperationStore, "replay")
	events, err := d.store.Replay(ctx, d.key, from)
	span.End(observeOutcome(err))
	return events, err
}

func (d *durableSink) start(kind observe.OperationKind, name string) (context.Context, observe.Handle) {
	ctx := d.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	o := d.observer
	if o == nil {
		o = observe.NoopObserver{}
	}
	return o.Start(ctx, observe.Descriptor{Kind: kind, Name: name})
}

func observeOutcome(err error) observe.Outcome {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return observe.OutcomeTimeout
	case errors.Is(err, context.Canceled):
		return observe.OutcomeCanceled
	case err != nil:
		return observe.OutcomeError
	default:
		return observe.OutcomeSuccess
	}
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
