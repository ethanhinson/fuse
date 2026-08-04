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
}

// New builds an Agent. modelID is the gateway model id; systemPrompt, when
// non-empty, is injected as the first message of each run.
func New(m Completer, t ToolExecutor, r Renderer, modelID, systemPrompt string, maxTurns, maxTokens int) *Agent {
	if maxTurns <= 0 {
		maxTurns = 25
	}
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
