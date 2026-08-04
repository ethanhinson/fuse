package agent

import (
	"context"
	"errors"

	"github.com/ethanhinson/fuse/internal/model"
)

// ErrMaxTurns is returned when the loop exhausts its turn budget.
var ErrMaxTurns = errors.New("agent: max turns reached")

// ErrLoopDetected is returned when identical tool calls repeat past the limit.
var ErrLoopDetected = errors.New("agent: tool-call loop detected")

const loopLimit = 3

// Run executes the agent loop over history (whose last message is the user
// turn) and returns the extended history. The system prompt, if set, is
// injected as the first message but only for the transport, not persisted into
// the returned history's head beyond the first run.
func (a *Agent) Run(ctx context.Context, history []model.Message) ([]model.Message, error) {
	messages := history
	detector := newLoopDetector(loopLimit)

	for turn := 0; turn < a.maxTurns; turn++ {
		if err := ctx.Err(); err != nil {
			return messages, err
		}

		req := model.CompletionReq{
			Model:     a.modelID,
			Messages:  a.withSystem(messages),
			Tools:     a.tools.Schemas(),
			MaxTokens: a.maxTokens,
		}
		resp, err := a.model.Complete(ctx, req)
		if err != nil {
			a.renderer.Errorf("model error: %v", err)
			return messages, err
		}
		a.renderer.Tokens(resp.InputTokens, resp.OutputTokens)

		if resp.Content != "" {
			a.renderer.Assistant(resp.Content)
		}
		messages = append(messages, resp.AsMessage())

		if len(resp.ToolCalls) == 0 {
			return messages, nil
		}

		fps := make([]string, 0, len(resp.ToolCalls))
		for _, c := range resp.ToolCalls {
			fps = append(fps, fingerprint(c.Name, c.Arguments))
		}
		if detector.seen(fps) {
			a.renderer.Errorf("aborting: identical tool calls repeated %d times", loopLimit)
			return messages, ErrLoopDetected
		}

		for _, call := range resp.ToolCalls {
			a.renderer.ToolCall(call.Name, call.Arguments)
			res := a.tools.Execute(ctx, call.Name, call.Arguments)
			a.renderer.ToolResult(call.Name, res)
			messages = append(messages, model.Message{
				Role:       "tool",
				ToolCallID: call.ID,
				Name:       call.Name,
				Content:    res.Output,
			})
		}
	}
	return messages, ErrMaxTurns
}

// withSystem prepends the system prompt (if any) to the message slice sent to
// the model, without mutating the persisted history.
func (a *Agent) withSystem(messages []model.Message) []model.Message {
	if a.systemPrompt == "" {
		return messages
	}
	out := make([]model.Message, 0, len(messages)+1)
	out = append(out, model.Message{Role: "system", Content: a.systemPrompt})
	out = append(out, messages...)
	return out
}
