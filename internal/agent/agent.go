package agent

import (
	"context"
	"errors"
	"time"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/observe"
	"github.com/ethanhinson/fuse/internal/tools"
)

// Completer is the model transport the loop calls each turn.
type Completer interface {
	Complete(ctx context.Context, req model.CompletionReq) (model.CompletionResp, error)
}

// ToolExecutor advertises tool schemas and executes tool calls by name.
type ToolExecutor interface {
	Schemas() []model.ToolSchema
	Execute(ctx context.Context, name, args string) tools.Result
}

// Renderer receives loop events for display.
type Renderer interface {
	Assistant(text string)
	ToolCall(name, args string)
	ToolResult(name string, res tools.Result)
	Errorf(format string, a ...any)
	Tokens(input, output int) // called after each gateway round-trip
}

// Agent binds a model, a tool set, a renderer, and run limits.
type Agent struct {
	observer     observe.Observer
	model        Completer
	tools        ToolExecutor
	renderer     Renderer
	modelID      string
	systemPrompt string
	maxTurns     int
	maxTokens    int

	// ContextWindow is the model's context size in tokens; 0 uses the
	// default (128k). The loop prunes old tool results when the estimated
	// request approaches this budget.
	ContextWindow int

	// LoopApproval, when set, makes the doom-loop detector interactive: on a
	// trip (loopLimit consecutive byte-identical tool-call sets) the loop calls
	// it with a "possible loop" preview instead of aborting. Returning
	// (true, nil) forces the repeated call through and resets the detector so
	// the run continues; (false, nil) aborts with ErrLoopDetected. A nil hook
	// is the non-interactive posture: a trip aborts immediately. The agent
	// package stays transport-agnostic — cmd/fuse adapts its ApprovalFunc and
	// interactivity into this callback. See change 0038.
	LoopApproval func(ctx context.Context, preview string) (approved bool, err error)

	// stripSpawn, when set, is consulted once per inference request. When it
	// returns true, the spawn_agent schema is omitted from that turn's tool
	// list (active-cap or budget brake). It must be race-safe and must not be
	// cached across turns. See change 0033.
	stripSpawn func() bool

	// summarizer, when non-nil, enables Tier 2 anchored summarization (change
	// 0027): at the over-budget point the loop runs a bounded LLM summarization
	// pass over the old tool-result region before Tier-1 stub pruning. Nil ⇒
	// Tier 2 off, byte-identical to the pre-0027 Tier-1 path.
	summarizer *summarizer
	// segmentSink receives the raw pre-summarization region for archival (change
	// 0027 ships only the no-op default; #0030 implements a real sink). Never nil
	// once SetSummarizer is called — a nil sink argument installs the no-op.
	segmentSink SegmentSink

	// relevanceScorer ranks tool results for relevance-aware pruning (change
	// 0028). Always non-nil after New() — the always-on heuristic scorer is the
	// default; pruneOldToolResults never sees nil. context.relevance.heuristic:
	// false installs the pure-recency scorer (the no-op degeneration path), not
	// a nil.
	relevanceScorer RelevanceScorer
	// recencyFloor is the percentage of the protection budget reserved for the
	// guaranteed recency floor (change 0028). 0 ⇒ the spec default (50) via
	// recencyFloorPct(); an explicit value from config overrides.
	recencyFloor int

	// toolTimeout bounds a single leaf tool call. A hanging tool (e.g. a bash
	// command that never returns) must not block the whole agent loop forever.
	// 0 selects DefaultToolTimeout. Orchestration tools that legitimately run
	// for a long time (spawn_agent, pipeline_run — they await child agents) are
	// exempt via toolTimeoutExempt; capping them would break multi-agent runs.
	toolTimeout time.Duration

	// humanInjector, when set, is polled at the top of every turn: it drains this
	// node's human-message queue (ADR-0022) and injects a single batched user
	// message before the next model call. Nil ⇒ no human messaging, byte-identical
	// to the pre-0051 loop. The node self-pulls its own queue — no cross-goroutine
	// push into a running node, honoring ADR-0016's run-to-completion contract.
	humanInjector *HumanInjector

	// interactive, when true, turns the root loop's terminal path (a model response
	// with no tool calls) into a PARK instead of a return: the run blocks at the turn
	// boundary awaiting the next human message (humanInjector.Wait), so one loop_id
	// carries a full multi-turn conversation with server-authoritative history. The
	// loop still exits on ctx cancellation (client disconnect / shutdown) or when no
	// bus is wired. False ⇒ byte-identical single-task run-to-completion (ADR-0016).
	// Only meaningful with humanInjector set; a spawned child is never interactive.
	interactive bool

	// expectsSchema, when non-nil, is the JSON Schema the spawner declared for this
	// child's structured result (change 0042). When set, Run offers a synthesized
	// return_result tool (parameters = this schema) and treats a conforming
	// return_result call as terminal, writing the validated value into expectsSink.
	// Nil ⇒ no return_result tool, byte-identical to the pre-0042 loop.
	expectsSchema map[string]any
	// expectsSink receives the validated return_result value so the spawner (which
	// does not construct this Agent) can read the structured result back after Run.
	// It is the shared holder that closes the "spawner owns SpawnOpts.Expects but
	// the loop sees return_result" gap (change 0042). Non-nil iff expectsSchema is.
	expectsSink *ExpectsSink

	// eventSink is the loop event stream (change 0043). The loop emits a typed
	// event at every state transition via a.emit(...). Never nil after New (defaults
	// to event.NoopStore), so no emission point can nil-panic; a real store is wired
	// post-New via SetEventSink from cmd/fuse, exactly as segmentSink is threaded via
	// EnableSummarization. Emission is best-effort — an Append error is logged and the
	// turn continues, mirroring segmentSink.Archive.
	eventSink event.EventStore
	// eventTrace is captured from Run's active operation context. It is a portable,
	// validated carrier rather than an OTel type so event emission stays vendor-free.
	eventTrace *event.TraceCarrier
	// node identity for emitted events (matches the AgentNode in the spawn tree).
	// Set post-New via SetNodeIdentity from the site that knows the node (root at
	// shell.go, children in the spawn closure). Empty/zero on probe/one-shot paths —
	// harmless, the events just carry no node identity.
	nodeID   string
	parentID string
	depth    int
}

// SetObserver installs provider-neutral timing observation. Nil selects a no-op.
func (a *Agent) SetObserver(o observe.Observer) {
	if o == nil {
		o = observe.NoopObserver{}
	}
	a.observer = o
	if a.summarizer != nil {
		a.summarizer.setObserver(o)
	}
	if scorer, ok := a.relevanceScorer.(*classifierScorer); ok {
		scorer.setObserver(o)
	}
}

// modelOutcome maps transport termination to the bounded model-observation
// vocabulary. Every production model-call path uses this helper.
func modelOutcome(err error) observe.Outcome {
	if errors.Is(err, context.DeadlineExceeded) {
		return observe.OutcomeTimeout
	}
	if errors.Is(err, context.Canceled) {
		return observe.OutcomeCanceled
	}
	if err != nil {
		return observe.OutcomeError
	}
	return observe.OutcomeSuccess
}

// ExpectsSink is the shared holder through which a child Agent's loop hands the
// validated return_result value back to the spawner that requested it (change
// 0042). The spawner allocates one per Expects-carrying spawn, threads it into
// the child Agent via SetExpects, and reads Value()/Captured() after Run returns.
// It is deliberately tiny and single-goroutine: the loop writes it before Run
// returns and the spawner reads it after, so no lock is needed.
type ExpectsSink struct {
	captured bool
	value    any
}

// Captured reports whether the loop captured a validated return_result value.
func (s *ExpectsSink) Captured() bool { return s != nil && s.captured }

// Value returns the captured structured value (nil if none was captured).
func (s *ExpectsSink) Value() any {
	if s == nil {
		return nil
	}
	return s.value
}

// set records a captured value; called once by the loop on a valid return_result.
func (s *ExpectsSink) set(v any) {
	if s == nil {
		return
	}
	s.captured = true
	s.value = v
}

// SetExpects installs the structured-delegation schema + capture sink on this
// Agent (change 0042). When schema is non-nil the run offers a synthesized
// return_result tool and treats a conforming call as terminal, writing the
// validated value into sink. A nil schema is a no-op (leaves the pre-0042 loop
// untouched). Set once at build time, before Run. This is the seam the cmd-site
// child builders call with opts.Expects + the spawner-allocated sink so the
// structured value flows back to the spawner (option (a) in the plan).
func (a *Agent) SetExpects(schema map[string]any, sink *ExpectsSink) {
	if schema == nil {
		return
	}
	a.expectsSchema = schema
	a.expectsSink = sink
}

// SetHumanInjector wires the per-node human-message injector (ADR-0022). Passing
// nil is a no-op (the field stays nil). Set once at build time, before Run.
func (a *Agent) SetHumanInjector(inj *HumanInjector) { a.humanInjector = inj }

// SetInteractive enables persistent conversational mode on the root loop: at the
// terminal turn boundary (no tool calls) the run parks awaiting the next human
// message instead of returning (see the interactive field). It is a no-op posture
// unless a HumanInjector with a bus is also wired. Default false preserves the
// single-task run-to-completion contract for every existing binding.
func (a *Agent) SetInteractive(v bool) { a.interactive = v }

// SetMaxTurns overrides the per-run turn cap after construction. A value <= 0 means
// unlimited. An interactive (persistent conversational) loop MUST be uncapped: each
// resumed exchange consumes real turns, so a finite cap would end the whole
// conversation once total turns crossed it. The runtime lifts the cap here when it
// enables interactive mode, independent of any per-turn backstop a one-shot binding
// wants.
func (a *Agent) SetMaxTurns(n int) { a.maxTurns = n }

// DefaultToolTimeout bounds a single leaf tool call when no explicit timeout is
// configured. Chosen to comfortably cover slow-but-legitimate work (large web
// fetch, long build) while still breaking a truly hung call.
const DefaultToolTimeout = 120 * time.Second

// toolTimeoutExempt reports whether a tool is exempt from the per-tool-call
// timeout because it legitimately runs long by awaiting other agents. These are
// bounded by their own budgets/scheduler, the parent context, and (for
// pipelines) per-step controls — not by the leaf-tool watchdog.
func toolTimeoutExempt(name string) bool {
	switch name {
	case "spawn_agent", "pipeline_run":
		return true
	default:
		return false
	}
}

// SetToolTimeout sets the per-tool-call timeout for leaf tools. A non-positive
// value restores DefaultToolTimeout. Orchestration tools remain exempt.
func (a *Agent) SetToolTimeout(d time.Duration) {
	if d <= 0 {
		d = DefaultToolTimeout
	}
	a.toolTimeout = d
}

// recencyFloorPct returns the effective recency-floor percentage: the spec
// default (50) when unset, otherwise the configured value clamped to [0,100].
func (a *Agent) recencyFloorPct() int {
	if a.recencyFloor <= 0 {
		return defaultRecencyFloorPct
	}
	if a.recencyFloor > 100 {
		return 100
	}
	return a.recencyFloor
}

// defaultRecencyFloorPct is the spec default fraction of the protection budget
// reserved for the guaranteed recency floor.
const defaultRecencyFloorPct = 50

// SetSummarizer enables Tier 2 anchored summarization (change 0027). A non-nil
// s wires the bounded summarizer; sink receives the raw pre-summarization region
// (nil ⇒ the no-op default sink, which persists nothing and omits the recovery
// pointer). Passing a nil s leaves Tier 2 off. Mirrors SetStripSpawn. This takes
// the package-internal summarizer; call sites outside the package use
// EnableSummarization.
func (a *Agent) SetSummarizer(s *summarizer, sink SegmentSink) {
	a.summarizer = s
	if s != nil {
		s.setObserver(a.observer)
	}
	if sink == nil {
		sink = noopSegmentSink{}
	}
	a.segmentSink = sink
}

// EnableSummarization is the exported wiring entry point for Tier 2 (change
// 0027): it builds the bounded summarizer from c (a Completer decorated with
// the "summarizer" trace label at the call site), the resolved summarizer model
// id, and the output-token cap, then installs it with sink (nil ⇒ the no-op
// default). c must be non-nil; a nil c is a no-op (Tier 2 stays off).
func (a *Agent) EnableSummarization(c Completer, modelID string, maxOutput int, sink SegmentSink) {
	if c == nil {
		return
	}
	a.SetSummarizer(newSummarizer(c, modelID, maxOutput), sink)
}

// SetStripSpawn installs the per-turn spawn-strip predicate. Nil (default)
// leaves spawn_agent always visible.
func (a *Agent) SetStripSpawn(fn func() bool) { a.stripSpawn = fn }

// SetEventSink installs the loop event stream (change 0043). A nil sink installs
// the no-op default so emission stays safe. Called post-New from cmd/fuse, exactly
// as EnableSummarization threads the segment sink.
func (a *Agent) SetEventSink(s event.EventStore) {
	if s == nil {
		s = event.NoopStore{}
	}
	a.eventSink = s
}

// SetNodeIdentity records this agent's identity in the spawn tree so emitted
// events carry the node coordinates (matches AgentNode.ID/ParentID/Depth). Unset ⇒
// empty/zero, harmless on probe/one-shot paths.
func (a *Agent) SetNodeIdentity(nodeID, parentID string, depth int) {
	a.nodeID = nodeID
	a.parentID = parentID
	a.depth = depth
}

// emit is the single greppable event-emission helper (change 0043): every loop
// state transition calls a.emit(kind, turn, payload). It stamps node identity and
// turn onto the envelope, marshals the payload, and Appends best-effort — an error
// is surfaced to the renderer and the turn continues, mirroring segmentSink.Archive
// (loop.go). A nil payload emits an event with no body. The explicit per-call Kind
// keeps the emitted transition set auditable by grep, deliberately not a hidden
// central interceptor.
func (a *Agent) emit(kind event.Kind, turn int, payload any) {
	if a.eventSink == nil {
		return
	}
	ev := event.Event{
		NodeID:   a.nodeID,
		ParentID: a.parentID,
		Depth:    a.depth,
		Turn:     turn,
		Kind:     kind,
	}
	if a.eventTrace != nil {
		trace := *a.eventTrace
		ev.Trace = &trace
	}
	if payload != nil {
		raw, err := event.MarshalPayload(payload)
		if err != nil {
			if a.renderer != nil {
				a.renderer.Errorf("event emit marshal (%s) failed (non-fatal): %v", kind, err)
			}
			return
		}
		ev.Payload = raw
	}
	if err := a.eventSink.Append(ev); err != nil && a.renderer != nil {
		a.renderer.Errorf("event emit (%s) failed (non-fatal): %v", kind, err)
	}
}

// SetRelevanceScorer installs the tool-result relevance scorer used by
// relevance-aware pruning (change 0028). A nil s keeps/installs the always-on
// heuristic default, so the prune step never runs without a scorer. Mirrors
// SetStripSpawn.
func (a *Agent) SetRelevanceScorer(s RelevanceScorer) {
	if s == nil {
		s = defaultHeuristicScorer()
	}
	a.relevanceScorer = s
}

// SetRecencyFloorPct sets the percentage of the protection budget reserved for
// the guaranteed recency floor (change 0028). Out-of-range values are clamped
// by recencyFloorPct(); 0 keeps the spec default (50).
func (a *Agent) SetRecencyFloorPct(pct int) { a.recencyFloor = pct }

// DisableHeuristicRelevance installs the pure-recency scorer (change 0028),
// selecting the no-op degeneration path: pruning protects exactly the newest
// protectTokens, byte-identical to pre-0028. Used when context.relevance.
// heuristic is false.
func (a *Agent) DisableHeuristicRelevance() { a.relevanceScorer = recencyOnlyScorer() }

// EnableRelevanceClassifier installs the optional hybrid LLM classifier over the
// always-on heuristic (change 0028). c is a bounded Completer (typically a
// *model.Adapter decorated WithTraceLabel(..., "relevance-classifier")); modelID
// is the classifier model; batchSize/lo/hi come from config. A nil c or empty
// modelID is a no-op — the heuristic scorer stays in place. The heuristic used
// as the classifier's base is the current default (or the configured body-scan
// cap when ConfigureRelevance built one).
func (a *Agent) EnableRelevanceClassifier(c Completer, modelID string, batchSize int, lo, hi float64) {
	if c == nil || modelID == "" {
		return
	}
	base, ok := a.relevanceScorer.(*heuristicScorer)
	if !ok || base == nil {
		base = defaultHeuristicScorer()
	}
	a.relevanceScorer = newClassifierScorer(base, c, modelID, batchSize, lo, hi)
	a.relevanceScorer.(*classifierScorer).setObserver(a.observer)
}

// New builds an Agent. modelID is the gateway model id; systemPrompt, when
// non-empty, is injected as the first message of each run. maxTurns <= 0 means
// unlimited turns (the loop never returns ErrMaxTurns); a positive maxTurns
// caps the run. The old `<=0 ⇒ 25` coercion is retired — the context-aware
// backstop now lives at the call site in cmd/fuse. See change 0038.
func New(m Completer, t ToolExecutor, r Renderer, modelID, systemPrompt string, maxTurns, maxTokens int) *Agent {
	return &Agent{
		model:        m,
		tools:        t,
		renderer:     r,
		modelID:      modelID,
		systemPrompt: systemPrompt,
		maxTurns:     maxTurns,
		maxTokens:    maxTokens,
		// Relevance-aware pruning is always on (change 0028): the heuristic
		// scorer is the default so pruneOldToolResults never sees a nil scorer.
		relevanceScorer: defaultHeuristicScorer(),
		toolTimeout:     DefaultToolTimeout,
		// Event emission is always safe: the no-op store is the default so every
		// a.emit(...) call is a no-op until a real store is wired post-New (change
		// 0043). Mirrors the segment sink's no-op default.
		eventSink: event.NoopStore{},
		observer:  observe.NoopObserver{},
	}
}
