package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/observe"
)

// ErrMaxDepthExceeded is returned when a spawn would exceed MaxDepth.
var ErrMaxDepthExceeded = errors.New("agent: max spawn depth exceeded")

// ErrSpawnBudgetExhausted is returned when a spawn would exceed the tree-global
// spawn budget (AgentTree.SetMaxSpawns). It is the backstop behind the injected
// budget line: the model is told the budget each spawn, and if it spawns past
// the ceiling anyway this refuses rather than letting fan-out run away.
var ErrSpawnBudgetExhausted = errors.New("agent: spawn budget exhausted")

// ErrWorkflowQuotaExhausted is returned by a workflow spawn backstop when a
// spawn would exceed the workflow pool's total quota or depth limit (change
// 0034). It is the per-call race/batch safety net behind the per-turn schema
// strip: a model that emits a batch of spawns in one turn (all seeing the tool
// present at turn start) is still checked call-by-call, so the pool cannot be
// overshot within a turn.
var ErrWorkflowQuotaExhausted = errors.New("agent: workflow spawn quota exhausted")

// ErrTokenQuotaExhausted is returned by the spawn backstop when a spawn would
// proceed past a hard TOKEN quota — either the workflow pool's lifetime token
// quota (pool.tokens) for the spawning node's subtree, or the global session
// token ceiling (throughput.session_tokens) — both change 0036. It mirrors
// ErrSpawnBudgetExhausted's identity discipline: the per-turn schema strip
// (Visible) hides spawn_agent once the quota is hit, and this is the call-time
// race backstop for a spawn committed within a turn before the strip took hold.
// The quotas are permanent (the per-node token counters are append-only), so
// once exhausted the tool stays gone in scope — there is no mid-turn abort.
var ErrTokenQuotaExhausted = errors.New("agent: token quota exhausted")

// ErrNoStructuredResult is returned by AgentHandle.Result when the child produced
// no validated structured result — either the spawn carried no Expects schema, or
// the child's output did not match it (change 0024). The raw text is still
// available via Wait().Result; a mismatch never fails the spawn.
var ErrNoStructuredResult = errors.New("agent: no structured result (child output absent or did not match the expected schema)")

// SpawnOpts configures a child agent spawn.
type SpawnOpts struct {
	Label        string
	Task         string
	SystemPrompt string
	Tools        []string
	ModelID      string
	MaxTurns     int
	MaxTokens    int
	// Worker names a workflow worker type (change 0034); empty for freeform
	// spawns. The child builder resolves it to the worker's tool allowlist.
	Worker string
	// Expects, when non-nil, is a JSON Schema (a decoded schema document, e.g.
	// map[string]any) the parent wants the child's final result to conform to
	// (change 0024). It is an asymmetric hint, not a constraint: the spawner
	// injects a directive into the child's system prompt asking it to emit only
	// conforming JSON, and validates the child's output against it on return. A
	// mismatch NEVER fails the spawn — it degrades to free text with a note. Nil
	// preserves all prior behavior byte-for-byte.
	Expects any

	// expectsSink is the spawner-allocated holder through which the child Agent's
	// loop returns its validated return_result value (change 0042). spawnLocal
	// allocates it when Expects is a map[string]any and threads it here so each
	// cmd-site child builder can hand it to the Agent via a.SetExpects(opts.Expects,
	// opts.ExpectsSink) — the single seam that lets the loop's captured value reach
	// the spawner without changing ChildBuilder's (string, error) return. It is
	// unexported-by-accessor: builders read it via ExpectsSink(). Nil when the spawn
	// carries no map schema, in which case SetExpects is a no-op.
	expectsSink *ExpectsSink

	// rawErrSink is the spawner-allocated holder through which a child builder
	// reports the CHILD'S RAW run error (change 0044) — the un-collapsed error from
	// a.Run, before childResult swallows ErrMaxTurns/ErrLoopDetected into a "[stopped:
	// …]" partial-success string. The Spawner reads it to source the spawn.done
	// event's Err field, so the event's error signal matches the direct session-log
	// write's raw-error `kind` selection (byte-equivalence for 0043's log projection).
	// It is SEPARATE from SpawnDone.Err (the control/handle path, which stays nil on
	// the stop path to preserve the model-facing partial-success contract). Builders
	// write it via opts.RunErrSink().Set(rerr); nil-safe when unallocated.
	rawErrSink *RunErrSink
}

// RunErrSink is the spawner-allocated holder a child builder uses to report the
// child's raw run error to the Spawner for spawn.done emission (change 0044),
// distinct from the collapsed error the handle/model see. Nil-safe: Set on a nil
// sink is a no-op and Err returns nil, so a builder that never wires it (older
// builder, a test fixedBuilder) leaves the event's Err empty exactly as before.
type RunErrSink struct {
	mu  sync.Mutex
	err error
}

// Set records the child's raw run error. Nil-safe.
func (s *RunErrSink) Set(err error) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.err = err
	s.mu.Unlock()
}

// Err returns the recorded raw run error, or nil. Nil-safe.
func (s *RunErrSink) Err() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// ExpectsSink returns the spawner-allocated return_result capture holder for this
// spawn (change 0042), or nil when the spawn carries no structured-delegation
// schema. Child builders pass it into the child Agent via
// a.SetExpects(opts.Expects, opts.ExpectsSink()) so the loop's validated
// return_result value flows back to the spawner's result assembly.
func (o SpawnOpts) ExpectsSink() *ExpectsSink { return o.expectsSink }

// RunErrSink returns the spawner-allocated raw-run-error holder for this spawn
// (change 0044), or nil when unallocated. A child builder calls
// opts.RunErrSink().Set(rerr) with the raw error from a.Run so the Spawner's
// spawn.done event carries the same error signal the direct session-log write uses.
func (o SpawnOpts) RunErrSink() *RunErrSink { return o.rawErrSink }

// ExpectsSchema returns the decoded map[string]any expects schema for this spawn,
// or nil when Expects is unset or not a map (change 0042). Child builders pass it
// to a.SetExpects so the one-line wiring is uniform across all cmd sites without
// each repeating the type assertion.
func (o SpawnOpts) ExpectsSchema() map[string]any {
	if m, ok := o.Expects.(map[string]any); ok {
		return m
	}
	return nil
}

// SpawnDone carries the result of a completed child agent.
type SpawnDone struct {
	Result string
	Err    error
	// Structured is the parsed JSON value from the child's result when it matched
	// the SpawnOpts.Expects schema (change 0024); nil when no schema was set or
	// the result did not match. The match note also rides inside Result, so a
	// SpawnFunc adapter that only forwards Result still conveys the outcome to the
	// model; Structured is the programmatic channel reachable via Result().
	Structured any
}

// AgentHandle holds a reference to a spawned child agent. Cancellation goes
// through the tree (AgentNode.SetCancel / Tree.CancelNode), not the handle.
type AgentHandle struct {
	NodeID string
	Done   <-chan SpawnDone

	// memo memoizes the single SpawnDone the channel delivers, so Wait() and
	// Result() are each callable in any order without double-draining the buffered
	// channel (change 0024). It is a POINTER so AgentHandle stays copy-safe: the
	// value is passed/stored/ranged by value across the codebase, and copies must
	// share the same memoization (and not copy a sync.Once lock).
	memo *doneMemo
}

// doneMemo caches the SpawnDone received from an AgentHandle's channel.
type doneMemo struct {
	once   sync.Once
	cached SpawnDone
}

// waitOnce receives (once) the SpawnDone from the channel and caches it, so
// repeated or interleaved Wait()/Result() calls return the same value. A
// zero-value handle (no memo — e.g. a returned {} on error) yields a zero
// SpawnDone without blocking.
func (h *AgentHandle) waitOnce() SpawnDone {
	if h.memo == nil {
		if h.Done == nil {
			return SpawnDone{}
		}
		h.memo = &doneMemo{}
	}
	h.memo.once.Do(func() { h.memo.cached = <-h.Done })
	return h.memo.cached
}

// Wait blocks until the child agent completes and returns its result. It is
// drain-safe: callable alongside Result() in either order (memoized).
func (h *AgentHandle) Wait() SpawnDone { return h.waitOnce() }

// Result blocks until the child completes and returns the parsed structured
// value — non-nil only when the spawn carried an Expects schema AND the child's
// output matched it. It returns the child's run error if the spawn itself failed,
// or ErrNoStructuredResult when the child produced no structured result (no schema
// was requested, or the output did not validate — the raw text is still available
// via Wait().Result). It is drain-safe alongside Wait() (change 0024). This is the
// programmatic accessor a future consumer (change 0026) uses; the model-facing
// path never depends on it.
func (h *AgentHandle) Result() (any, error) {
	d := h.waitOnce()
	if d.Err != nil {
		return nil, d.Err
	}
	if d.Structured == nil {
		return nil, ErrNoStructuredResult
	}
	return d.Structured, nil
}

// ChildBuilder runs a local child agent. node is the child's already-created
// AgentNode in the tree. It returns the final result text.
type ChildBuilder func(ctx context.Context, opts SpawnOpts, node *AgentNode, tree *AgentTree) (string, error)

// Option configures a Spawner.
type Option func(*Spawner)

// WithTree sets the agent tree on a Spawner.
func WithTree(t *AgentTree) Option { return func(s *Spawner) { s.tree = t } }

// WithNode sets the current (parent) node on a Spawner.
func WithNode(n *AgentNode) Option { return func(s *Spawner) { s.node = n } }

// WithSpawnDepth sets the current spawn depth.
func WithSpawnDepth(depth int) Option { return func(s *Spawner) { s.depth = depth } }

// WithChildBuilder injects the local child-agent runner into a Spawner.
func WithChildBuilder(fn ChildBuilder) Option { return func(s *Spawner) { s.buildChild = fn } }

// WithSpawnBackstop installs an optional per-call workflow backstop, invoked
// after the global depth/budget checks with the child's would-be depth. A
// non-nil error refuses the spawn (change 0034). Nil (default) is a no-op.
func WithSpawnBackstop(fn func(newDepth int) error) Option {
	return func(s *Spawner) { s.backstop = fn }
}

// WithHumanBus installs the human-message bus so a spawned child's undelivered
// queue is bubbled to its parent when the child finishes (ADR-0022 completion
// hook). Nil (default) is a no-op.
func WithHumanBus(bus *HumanBus) Option { return func(s *Spawner) { s.humanBus = bus } }

// WithEventStore installs the loop event stream on the Spawner so it emits the
// spawn.start / spawn.done lifecycle pair for every child (change 0044). The
// Spawner is the single choke point every spawn passes through and already builds
// SpawnDone{Result,Structured}, so both the in-process backend and any future
// networked backend emit uniformly from here — rather than from each cloned
// cmd-site child builder (where 0043 first wired it). A nil store installs the
// inert NoopStore, so a Spawner without one emits into the void (one-shot / probe
// / mcp paths, and every existing test) exactly as before.
func WithEventStore(s event.EventStore) Option {
	return func(sp *Spawner) {
		if s == nil {
			s = event.NoopStore{}
		}
		sp.eventStore = s
	}
}

func WithObserver(o observe.Observer) Option {
	return func(s *Spawner) {
		if o == nil {
			o = observe.NoopObserver{}
		}
		s.observer = o
	}
}

// Spawner provides the Spawn method for creating child agents.
type Spawner struct {
	tree       *AgentTree
	node       *AgentNode
	depth      int
	buildChild ChildBuilder
	backstop   func(newDepth int) error
	humanBus   *HumanBus
	// eventStore is the loop event stream (change 0044). The Spawner emits the
	// spawn.start / spawn.done lifecycle pair here — the observation path that lets
	// an observer see child completion without holding the handle. Defaults to the
	// inert NoopStore so a Spawner without WithEventStore never nil-panics.
	eventStore event.EventStore
	observer   observe.Observer
}

// NewSpawner creates a Spawner with the provided options.
func NewSpawner(opts ...Option) *Spawner {
	s := &Spawner{eventStore: event.NoopStore{}, observer: observe.NoopObserver{}}
	for _, o := range opts {
		o(s)
	}
	if s.eventStore == nil {
		s.eventStore = event.NoopStore{}
	}
	return s
}

// Spawn creates a child agent running locally in a goroutine.
func (s *Spawner) Spawn(ctx context.Context, opts SpawnOpts) (h AgentHandle, err error) {
	ctx, span := s.observer.Start(ctx, observe.Descriptor{Kind: observe.OperationSpawn, Name: "child", Fields: []observe.Field{{Key: "worker", Value: opts.Worker}}})
	async := false
	defer func() {
		if async {
			return
		}
		out := observe.OutcomeSuccess
		if errors.Is(err, context.Canceled) {
			out = observe.OutcomeCanceled
		} else if errors.Is(err, context.DeadlineExceeded) {
			out = observe.OutcomeTimeout
		} else if err != nil {
			out = observe.OutcomeError
		}
		span.End(out)
	}()
	newDepth := s.depth + 1
	if newDepth > MaxDepth {
		return AgentHandle{}, fmt.Errorf("%w: depth %d > %d", ErrMaxDepthExceeded, newDepth, MaxDepth)
	}
	// Tree-global spawn budget backstop. The injected budget line should make
	// this rare; the refusal makes runaway fan-out impossible. Only enforced
	// when a budget is configured (max > 0).
	if s.tree != nil {
		if used, max := s.tree.SpawnBudget(); max > 0 && used >= max {
			return AgentHandle{}, fmt.Errorf("%w: %d/%d spawns used — proceed with the results you already have and do not spawn again", ErrSpawnBudgetExhausted, used, max)
		}
		// Queue-bound backstop (change 0036): a spawn that commits within a turn
		// while its pool queue has since filled to the bound is refused rather than
		// growing the queue past ceil(queue_bound × pool slots). The per-turn strip
		// (Task 3) makes this rare; this is the race safety net.
		poolID := ""
		if s.node != nil {
			poolID = s.tree.WorkflowRootOf(s.node.ID)
		}
		if v, _ := s.tree.Scheduler().Admit(AdmitRequest{Depth: s.depth, PoolID: poolID}); v == OverBound {
			return AgentHandle{}, fmt.Errorf("%w: pool queue at bound — proceed with the results you already have", ErrQueueBoundExceeded)
		}
		// Token-quota backstop (change 0036): the call-time race safety net behind
		// the per-turn strip for the session ceiling and the workflow pool.tokens
		// quota. A spawn committed within a turn before the strip took hold is
		// refused rather than allowed to spend past a hard token ceiling.
		nodeID := ""
		if s.node != nil {
			nodeID = s.node.ID
		}
		if err := s.tree.Scheduler().tokenQuotaDenied(nodeID); err != nil {
			return AgentHandle{}, err
		}
	}
	// Workflow pool backstop (change 0034): the per-call safety net behind the
	// per-turn strip, so a within-turn batch cannot overshoot the pool.
	if s.backstop != nil {
		if err := s.backstop(newDepth); err != nil {
			return AgentHandle{}, err
		}
	}
	h, err = s.spawnLocal(ctx, opts, newDepth, span)
	async = err == nil
	return h, err
}

func (s *Spawner) spawnLocal(ctx context.Context, opts SpawnOpts, depth int, span observe.Handle) (AgentHandle, error) {
	parentID := ""
	if s.node != nil {
		parentID = s.node.ID
	}

	// Slot admission goes through the scheduler (change 0036); a nil tree (a
	// Spawner used without one) has no scheduler and thus no cap. The child queues
	// into its parent's pool — the parent's workflow root, or "" for the implicit
	// session pool (a freshly-spawned child never carries its own workflow-root
	// label, so it inherits the parent's, matching the Spawn-time Admit check).
	var sched *Scheduler
	poolID := ""
	if s.tree != nil {
		sched = s.tree.Scheduler()
		if s.node != nil {
			poolID = s.tree.WorkflowRootOf(s.node.ID)
		}
	}

	// Enqueue for a slot SYNCHRONOUSLY, before the node is added to the tree, so a
	// queue-bound refusal surfaces from Spawn identically to the Admit backstop
	// (SF-1): the atomic under-lock bound check closes the TOCTOU window that lets
	// concurrent same-turn spawns overshoot the pool bound. A refused spawn adds no
	// pending node. granted==true means a free slot was taken here (fast path);
	// otherwise the returned waiter is parked and the goroutine awaits its grant.
	waiter, granted, err := sched.enqueueSlot(poolID, false)
	if err != nil {
		return AgentHandle{}, err
	}
	// SF-2 (slot-leak safety): once enqueueSlot has granted a slot, slotsInUse is
	// already incremented. If anything below panics before the worker goroutine
	// takes ownership of the release, the slot would leak permanently and shrink
	// the global concurrency cap. Guard the synchronous setup so a granted slot is
	// handed back on panic; slotHeld is cleared once the goroutine owns release.
	slotHeld := granted
	defer func() {
		if slotHeld {
			sched.releaseSlot()
		}
	}()

	node := &AgentNode{
		ID:       newNodeID(),
		ParentID: parentID,
		Label:    opts.Label,
		Model:    opts.ModelID,
		Status:   StatusPending,
		Depth:    depth,
	}
	doneCh := make(chan SpawnDone, 1)
	childCtx, cancel := context.WithCancel(ctx)
	// SF-3 (cancel race): store the cancel func BEFORE the node is added to the
	// tree, so a concurrent tree.CancelNode that discovers the node can never read
	// a nil cancel func. addNode is the point of exposure; SetCancel must precede
	// it. (Was: addNode then SetCancel, leaving a nil-cancel window.)
	node.SetCancel(cancel) // allows tree.CancelNode to stop this node
	if s.tree != nil {
		s.tree.addNode(node)
		s.tree.Emit(TreeUpdate{NodeID: node.ID})
	}

	// spawn.start (change 0044): the spawn boundary opens HERE, at admission, from
	// the single Spawner choke point — before the child goroutine runs — so an
	// observer sees the start of every spawn's lifecycle whether the backend is
	// in-process or (later) networked. Best-effort; never blocks (ADR-0016).
	s.emitSpawnStart(node, opts.Task)

	// The goroutine now owns slot release for both the granted (fast-path) and
	// parked (awaitSlot) cases; the synchronous guard above no longer applies.
	slotHeld = false
	go func() {
		defer cancel()
		// If the slot was already granted synchronously, release it when this
		// goroutine exits. For the parked case the release defer is registered
		// after awaitSlot succeeds, so a cancelled waiter never over-releases.
		if granted {
			defer sched.releaseSlot()
		}

		// Width cap: wait for the granted slot (or park-and-wait) while the node
		// stays visibly pending. Depth limits alone don't bound load when the model
		// fans out widely. The slot was reserved synchronously above; here we only
		// await the parked waiter's grant when one was queued.
		if !granted {
			if err := sched.awaitSlot(childCtx, waiter); err != nil {
				node.Finish(StatusCancelled, "")
				if s.tree != nil {
					s.tree.Emit(TreeUpdate{NodeID: node.ID})
				}
				doneCh <- SpawnDone{Err: childCtx.Err()}
				span.End(observe.OutcomeCanceled)
				return
			}
			defer sched.releaseSlot()
		}

		node.mu.Lock()
		node.Status = StatusRunning
		node.StartedAt = time.Now()
		node.mu.Unlock()
		if s.tree != nil {
			s.tree.Emit(TreeUpdate{NodeID: node.ID})
		}

		var result string
		var runErr error

		// Producer side (change 0042, supersedes 0024's directive): when the parent
		// declared an expected result schema, install the structured-output channel.
		// If the schema is a map[string]any (the real case), allocate the capture
		// sink and inject a SHORT, non-contradictory hint naming the synthesized
		// return_result tool — NOT the old "final message = only JSON" directive,
		// which competed with the tool channel and caused the result object to be
		// crammed into a working tool's arguments (the bug this change fixes). The
		// sink is threaded via opts.expectsSink so each cmd-site child builder can
		// call a.SetExpects(opts.Expects, opts.ExpectsSink()); the loop offers the
		// return_result tool, validates it, and writes the captured value here.
		//
		// Fallback (D5, back-compat): a non-map Expects (or a child builder that
		// never wires SetExpects and never calls return_result — e.g. the fixedBuilder
		// in tests) leaves no sink capture; result assembly below then falls back to
		// the lenient final-message validation exactly as pre-0042, so behavior is
		// never worse than today and existing spawn_expects tests stay green.
		var sink *ExpectsSink
		if opts.Expects != nil {
			if schema, ok := opts.Expects.(map[string]any); ok {
				sink = &ExpectsSink{}
				opts.expectsSink = sink
				opts.SystemPrompt = augmentPromptWithReturnResult(opts.SystemPrompt, schema)
			} else {
				// Non-map schema: no return_result tool is synthesizable; retain the
				// legacy directive path so the lenient fallback can still validate.
				opts.SystemPrompt = augmentPromptWithSchema(opts.SystemPrompt, opts.Expects)
			}
		}

		// Raw-run-error channel (change 0044): the child builder reports the
		// un-collapsed a.Run error here so the spawn.done event's Err matches the
		// direct session-log write's raw-error `kind` selection (byte-equivalence for
		// 0043's projection). Distinct from SpawnDone.Err, which stays the collapsed
		// control-path error the handle/model see.
		rawErrSink := &RunErrSink{}
		opts.rawErrSink = rawErrSink

		if s.buildChild != nil {
			// Backstop: a panic on a child goroutine kills the whole process,
			// TUI included. Convert it into a child error instead.
			func() {
				defer func() {
					if rec := recover(); rec != nil {
						stack := debug.Stack()
						if len(stack) > 2048 {
							stack = stack[:2048]
						}
						runErr = fmt.Errorf("child agent panicked: %v\n%s", rec, stack)
					}
				}()
				result, runErr = s.buildChild(childCtx, opts, node, s.tree)
			}()
		}

		// Result side (change 0024): when a schema was expected and the child
		// succeeded, leniently extract + validate the JSON. On match, populate
		// Structured and append the match note. On mismatch, NEVER fail the spawn —
		// keep the raw text, append the mismatch note, and record a labeled tree
		// event. Done in the child goroutine (off the parent's slot), before the
		// channel send, so the note rides inside Result and Structured on the same
		// SpawnDone.
		//
		// Change 0042: the structured result now comes primarily from the validated
		// return_result call the loop captured into `sink`. When one was captured we
		// use it directly (no re-validation, no note mutation — the human-facing text
		// stays whatever the child last said). When NONE was captured (a child that
		// never called return_result: an older prompt, a fixedBuilder in tests, or a
		// model that chose the message channel), fall back to the lenient
		// final-message validation EXACTLY as pre-0042 (D5) — keeping the match /
		// mismatch notes and the schema_mismatch tree event.
		var structured any
		if opts.Expects != nil && runErr == nil {
			if sink.Captured() {
				structured = sink.Value()
			} else if schema, ok := opts.Expects.(map[string]any); ok {
				parsed, verr := validateAgainstSchema(schema, result)
				if verr == nil {
					structured = parsed
					result += "\n\n(matched expected schema)"
				} else {
					result += "\n\n(result did NOT match expected schema: " + verr.Error() + ")"
					node.AddEvent(AgentEvent{
						Kind:    KindError,
						Name:    "schema_mismatch",
						Payload: map[string]any{"error": verr.Error()},
						TS:      time.Now(),
					})
				}
			}
		}

		if runErr != nil {
			if errors.Is(runErr, context.Canceled) {
				node.Finish(StatusCancelled, "")
			} else {
				node.Finish(StatusError, runErr.Error())
			}
		} else {
			node.Finish(StatusDone, "")
		}
		if s.tree != nil {
			s.tree.Emit(TreeUpdate{NodeID: node.ID})
		}
		// Completion hook (ADR-0022): bubble any messages the human queued for this
		// child that were never drained (e.g. its final turn emitted no tool call,
		// so Poll never fired) up to the parent, so none are silently stranded.
		if s.humanBus != nil {
			s.humanBus.OnNodeComplete(node.ID)
		}
		// spawn.done (change 0044): emit the completion on the event stream from the
		// same place SpawnDone is assembled, carrying the child's Result / Err /
		// Structured — the observation path a Subscribe()r sees without a handle. The
		// handle channel below stays the in-process control path (D2); this emission
		// is a best-effort side effect of the same completion, never read back by the
		// handle. Emitted before the channel send so a same-goroutine ordering is
		// deterministic for tests.
		//
		// The event's Err is the child's RAW run error (rawErrSink) when the builder
		// reported one — so the projected session log's `kind` matches the direct
		// write's raw-error selection even on the max-turns/loop path where the
		// control-path runErr was collapsed to nil (a partial-success string). Falls
		// back to runErr when no sink error was reported (real failure, or a builder
		// that never wires the sink).
		eventErr := runErr
		if re := rawErrSink.Err(); re != nil {
			eventErr = re
		}
		s.emitSpawnDone(node, result, eventErr, structured)
		out := observe.OutcomeSuccess
		if errors.Is(eventErr, context.Canceled) {
			out = observe.OutcomeCanceled
		} else if errors.Is(eventErr, context.DeadlineExceeded) {
			out = observe.OutcomeTimeout
		} else if eventErr != nil {
			out = observe.OutcomeError
		}
		span.End(out)
		doneCh <- SpawnDone{Result: result, Err: runErr, Structured: structured}
	}()

	return AgentHandle{NodeID: node.ID, Done: doneCh, memo: &doneMemo{}}, nil
}

// emitSpawnStart appends a spawn.start event for the given child node (change
// 0044). Best-effort: a nil/absent store is the inert NoopStore, and an Append
// error is swallowed (the loop's own event emission has the same posture,
// ADR-0016) — spawning must never fail on an observation-channel hiccup.
func (s *Spawner) emitSpawnStart(node *AgentNode, task string) {
	if s.eventStore == nil || node == nil {
		return
	}
	pl, err := event.MarshalPayload(event.SpawnStartPayload{
		ChildNodeID: node.ID,
		Label:       node.Label,
		Task:        task,
	})
	if err != nil {
		return
	}
	_ = s.eventStore.Append(event.Event{
		NodeID:   node.ID,
		ParentID: node.ParentID,
		Depth:    node.Depth,
		Kind:     event.KindSpawnStart,
		Payload:  pl,
	})
}

// emitSpawnDone appends a spawn.done event carrying the child's final Result, its
// run error (if any), and the structured-delegation value (change 0044). The
// Result is the child builder's returned string (i.e. SpawnDone.Result), which on
// max-turns / loop-detected carries a "[stopped: …]" marker — a faithful record of
// what the child ultimately produced. The 0043 log projection reads only the
// node-identity fields from this payload, so the projected session log stays
// byte-identical regardless of the Result source. Structured (any) is marshaled to
// raw JSON; a marshal failure just omits it. Best-effort, like emitSpawnStart.
func (s *Spawner) emitSpawnDone(node *AgentNode, result string, rerr error, structured any) {
	if s.eventStore == nil || node == nil {
		return
	}
	errStr := ""
	if rerr != nil {
		errStr = rerr.Error()
	}
	var raw json.RawMessage
	if structured != nil {
		if b, merr := json.Marshal(structured); merr == nil {
			raw = b
		}
	}
	pl, err := event.MarshalPayload(event.SpawnDonePayload{
		ChildNodeID: node.ID,
		ParentID:    node.ParentID,
		Label:       node.Label,
		Depth:       node.Depth,
		Result:      result,
		Err:         errStr,
		Structured:  raw,
	})
	if err != nil {
		return
	}
	_ = s.eventStore.Append(event.Event{
		NodeID:   node.ID,
		ParentID: node.ParentID,
		Depth:    node.Depth,
		Kind:     event.KindSpawnDone,
		Payload:  pl,
	})
}
