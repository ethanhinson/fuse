package agent

import (
	"context"
	"time"

	"github.com/ethanhinson/fuse/internal/model"
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
}

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
	}
}
