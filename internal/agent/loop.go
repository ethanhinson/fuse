package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/ethanhinson/fuse/internal/model"
)

// ErrMaxTurns is returned when the loop exhausts its turn budget.
var ErrMaxTurns = errors.New("agent: max turns reached")

// ErrLoopDetected is returned when identical tool calls repeat past the limit.
var ErrLoopDetected = errors.New("agent: tool-call loop detected")

const loopLimit = 3

// readOnlyTools are tools that read state without side effects. Redundant
// calls to these are intercepted by the compatibility layer.
var readOnlyTools = map[string]bool{
	"read_file":         true,
	"list_directory":    true,
	"codeindex_impact":  true,
	"codeindex_callers": true,
}

// Run executes the agent loop over history (whose last message is the user
// turn) and returns the extended history. The system prompt, if set, is
// injected as the first message but only for the transport, not persisted into
// the returned history's head beyond the first run.
func (a *Agent) Run(ctx context.Context, history []model.Message) ([]model.Message, error) {
	messages := history
	detector := newLoopDetector(loopLimit)
	// seenCalls tracks (name, args) fingerprints already executed this session.
	// Used by the compatibility layer to detect and interrupt redundant reads.
	seenCalls := map[string]int{} // fp → count of times executed
	// forceTextNext is set after redundant reads to force a text response on
	// the following turn via tool_choice:"none". Reset after one use.
	forceTextNext := false

	for turn := 0; turn < a.maxTurns; turn++ {
		if err := ctx.Err(); err != nil {
			return messages, err
		}

		// Compat layer: on a forced-text turn, send NO tool schemas so the model
		// cannot call any tools regardless of whether it respects tool_choice.
		// Also bypass the loop detector on this turn — if the model still manages
		// to emit tool calls (shouldn't be possible) we handle it next iteration.
		isCompatTurn := forceTextNext
		forceTextNext = false

		var toolSchemas []model.ToolSchema
		if !isCompatTurn {
			toolSchemas = a.tools.Schemas()
		}
		req := model.CompletionReq{
			Model:     a.modelID,
			Messages:  a.withSystem(messages),
			Tools:     toolSchemas,
			MaxTokens: a.maxTokens,
		}
		resp, err := a.model.Complete(ctx, req)
		if err != nil {
			a.renderer.Errorf("model error: %v", err)
			return messages, err
		}

		if resp.Content != "" {
			a.renderer.Assistant(resp.Content)
		}
		messages = append(messages, resp.AsMessage())

		if len(resp.ToolCalls) == 0 {
			return messages, nil
		}

		// On a compat turn, if the model somehow still called tools (model bug),
		// treat it as an unrecoverable loop — the compat layer has been exhausted.
		if isCompatTurn {
			a.renderer.Errorf("aborting: model called tools despite compat intervention (no schemas sent)")
			return messages, ErrLoopDetected
		}

		fps := make([]string, 0, len(resp.ToolCalls))
		for _, c := range resp.ToolCalls {
			fps = append(fps, fingerprint(c.Name, c.Arguments))
		}
		if detector.seen(fps) {
			a.renderer.Errorf("aborting: identical tool calls repeated %d times", loopLimit)
			return messages, ErrLoopDetected
		}

		anyRedundant := false
		for _, call := range resp.ToolCalls {
			fp := fingerprint(call.Name, call.Arguments)
			seenCalls[fp]++
			isRedundant := readOnlyTools[call.Name] && seenCalls[fp] > 1

			a.renderer.ToolCall(call.Name, call.Arguments)
			var content string
			if isRedundant {
				anyRedundant = true
				// Re-execute so the tool result slot is still properly filled,
				// but prefix it with a compat hint so even weak models notice.
				res := a.tools.Execute(ctx, call.Name, call.Arguments)
				a.renderer.ToolResult(call.Name, res)
				content = fmt.Sprintf(
					"[Already retrieved (call #%d). The result below is identical to what is already in your conversation history. "+
						"Once all tool results are returned, write your response as text — do not call this tool again.]\n\n%s",
					seenCalls[fp], res.Output,
				)
			} else {
				res := a.tools.Execute(ctx, call.Name, call.Arguments)
				a.renderer.ToolResult(call.Name, res)
				content = res.Output
			}
			messages = append(messages, model.Message{
				Role:       "tool",
				ToolCallID: call.ID,
				Name:       call.Name,
				Content:    content,
			})
		}

		// Compatibility layer: when any read-only call was redundant, two signals
		// fire together to break models stuck in a tool-call loop:
		//   1. A user-role nudge (higher-weight than system prompt for weak models)
		//   2. tool_choice:"none" on the next request — removes the ability to
		//      call tools at all, forcing a text response. Models that ignore
		//      instructional nudges cannot ignore a missing tool schema.
		if anyRedundant {
			messages = append(messages, model.Message{
				Role: "user",
				Content: "You have already retrieved that content — it is visible in your conversation history above. " +
					"Please write your response as text now. Do not call any more tools.",
			})
			forceTextNext = true
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
