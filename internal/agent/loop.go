package agent

import (
	"context"
	"errors"
	"sync"

	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/tools"
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

		toolMsgs := a.executeTools(ctx, resp.ToolCalls)
		messages = append(messages, toolMsgs...)
	}
	return messages, ErrMaxTurns
}

// toolResult pairs a call with its outcome, preserving order for the history.
type toolResult struct {
	call model.ToolCall
	res  tools.Result
}

// executeTools runs the tool calls from a single model response. When the
// response contains 2+ spawn_agent calls they run concurrently; all other
// combinations execute sequentially to avoid tool-state conflicts.
func (a *Agent) executeTools(ctx context.Context, calls []model.ToolCall) []model.Message {
	results := make([]toolResult, len(calls))

	// Parallel path: 2+ spawn_agent calls can safely run concurrently.
	// Each goroutine announces its own ToolCall before starting Execute so the
	// TUI has the label entry in inlineByLabel before tree updates fire.
	allSpawn := len(calls) >= 2
	for _, c := range calls {
		if c.Name != "spawn_agent" {
			allSpawn = false
			break
		}
	}

	if allSpawn {
		var wg sync.WaitGroup
		for i, call := range calls {
			wg.Add(1)
			go func(i int, call model.ToolCall) {
				defer wg.Done()
				a.renderer.ToolCall(call.Name, call.Arguments)
				results[i] = toolResult{call: call, res: a.tools.Execute(ctx, call.Name, call.Arguments)}
			}(i, call)
		}
		wg.Wait()
	} else {
		for i, call := range calls {
			a.renderer.ToolCall(call.Name, call.Arguments)
			results[i] = toolResult{call: call, res: a.tools.Execute(ctx, call.Name, call.Arguments)}
		}
	}

	msgs := make([]model.Message, 0, len(calls))
	for _, r := range results {
		a.renderer.ToolResult(r.call.Name, r.res)
		msgs = append(msgs, model.Message{
			Role:       "tool",
			ToolCallID: r.call.ID,
			Name:       r.call.Name,
			Content:    r.res.Output,
		})
	}
	return msgs
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
