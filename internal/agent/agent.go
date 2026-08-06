package agent

import (
	"context"

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
}

// SetStripSpawn installs the per-turn spawn-strip predicate. Nil (default)
// leaves spawn_agent always visible.
func (a *Agent) SetStripSpawn(fn func() bool) { a.stripSpawn = fn }

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
	}
}
