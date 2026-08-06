package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/tools"
)

// ErrMaxTurns is returned when the loop exhausts its turn budget.
var ErrMaxTurns = errors.New("agent: max turns reached")

// ErrLoopDetected is returned when identical tool calls repeat past the limit.
var ErrLoopDetected = errors.New("agent: tool-call loop detected")

// ErrContextTooLarge is returned only as a last resort: when even pruning
// old tool results cannot bring the conversation under the context budget.
var ErrContextTooLarge = errors.New("agent: conversation context too large")

const loopLimit = 3

// Context budgeting (see docs/designs/context-management.md). Token counting
// is hybrid: the provider-reported usage of the last response is exact; only
// the delta appended since is estimated at bytes/4, so the estimate resets to
// ground truth every turn and error never compounds.
const (
	// defaultContextWindow is used when the model config doesn't set one.
	defaultContextWindow = 128_000
	bytesPerToken        = 4
	// pruneThresholdPct: prune when the estimated request exceeds this
	// percentage of the context window.
	pruneThresholdPct = 85
	// pruneProtectTokens caps how many estimated tokens of the newest tool
	// results are protected from pruning (recency protection). The effective
	// protection also scales down with small context windows.
	pruneProtectTokens = 40_000
)

// protectBudget returns the recency-protection budget for a window: a
// quarter of the window, capped at pruneProtectTokens. recovery mode (after
// a provider length rejection) protects a quarter of that.
func protectBudget(window int, recovery bool) int {
	p := window / 4
	if p > pruneProtectTokens {
		p = pruneProtectTokens
	}
	if recovery {
		p /= 4
	}
	return p
}

// prunedStub replaces cleared tool results. The tool call (name + args)
// stays in history, so the model still sees WHAT it did — just not the body.
const prunedStub = "[old tool result cleared to free context — re-run the tool if needed]"

// messagesSize estimates the payload of msgs in bytes.
func messagesSize(msgs []model.Message) int {
	total := 0
	for _, m := range msgs {
		total += len(m.Content)
		for _, tc := range m.ToolCalls {
			total += len(tc.Arguments)
		}
	}
	return total
}

// pruneOldToolResults stubs tool results outside the protected recent tail
// and returns the estimated tokens freed. Only "tool" role messages are
// touched — user intent and assistant tool_calls stay intact, so provider
// tool pairing remains valid.
func pruneOldToolResults(messages []model.Message, protectTokens int) int {
	freed, seen := 0, 0
	for i := len(messages) - 1; i >= 0; i-- {
		m := &messages[i]
		if m.Role != "tool" || m.Content == prunedStub {
			continue
		}
		tok := len(m.Content) / bytesPerToken
		if seen < protectTokens {
			seen += tok
			continue
		}
		freed += tok
		m.Content = prunedStub
	}
	return freed
}

// isContextLengthErr reports whether a gateway error is a context-length
// rejection (patterns vary per upstream provider behind LiteLLM).
func isContextLengthErr(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	for _, pat := range []string{
		"context length", "context_length", "context window", "too long",
		"maximum context", "token limit", "tokens exceed", "input is too long",
	} {
		if strings.Contains(s, pat) {
			return true
		}
	}
	return false
}

// Run executes the agent loop over history (whose last message is the user
// turn) and returns the extended history. The system prompt, if set, is
// injected as the first message but only for the transport, not persisted into
// the returned history's head beyond the first run.
func (a *Agent) Run(ctx context.Context, history []model.Message) ([]model.Message, error) {
	messages := history
	detector := newLoopDetector(loopLimit)

	window := a.ContextWindow
	if window <= 0 {
		window = defaultContextWindow
	}
	budget := window * pruneThresholdPct / 100

	// Hybrid token accounting: exact usage from the last response, plus a
	// bytes/4 estimate of only the messages appended since.
	lastUsage := 0
	accounted := 0 // messages[:accounted] are covered by lastUsage
	retriedContext := false

	// a.maxTurns <= 0 means unlimited: the loop runs until the model stops
	// calling tools, context is exhausted, or the doom-loop detector trips.
	// A positive maxTurns caps the run and returns ErrMaxTurns. See change 0038.
	for turn := 0; a.maxTurns <= 0 || turn < a.maxTurns; turn++ {
		if err := ctx.Err(); err != nil {
			return messages, err
		}

		estimate := lastUsage + messagesSize(messages[accounted:])/bytesPerToken
		if estimate > budget {
			freed := pruneOldToolResults(messages, protectBudget(window, false))
			if freed > 0 {
				a.renderer.Errorf("context: ~%dk/%dk tokens — cleared ~%dk of old tool results", estimate/1000, window/1000, freed/1000)
			}
			estimate -= freed
			if estimate > budget {
				err := fmt.Errorf("%w: ~%dk tokens against a %dk window even after pruning — delegate to spawn_agent children or start a fresh session",
					ErrContextTooLarge, estimate/1000, window/1000)
				a.renderer.Errorf("%v", err)
				return messages, err
			}
		}

		req := model.CompletionReq{
			Model:     a.modelID,
			Messages:  a.withSystem(messages),
			Tools:     a.tools.Schemas(),
			MaxTokens: a.maxTokens,
		}
		resp, err := a.model.Complete(ctx, req)
		if err != nil {
			// The estimate said we fit but the provider disagreed: prune hard
			// (deterministic — recovery must not depend on another LLM call)
			// and retry exactly once.
			if isContextLengthErr(err) && !retriedContext {
				retriedContext = true
				if freed := pruneOldToolResults(messages, protectBudget(window, true)); freed > 0 {
					a.renderer.Errorf("context: gateway rejected for length; cleared ~%dk tokens of old tool results, retrying", freed/1000)
					turn--
					continue
				}
			}
			a.renderer.Errorf("model error: %v", err)
			return messages, err
		}
		a.renderer.Tokens(resp.InputTokens, resp.OutputTokens)
		lastUsage = resp.InputTokens + resp.OutputTokens

		if resp.Content != "" {
			a.renderer.Assistant(resp.Content)
		}
		messages = append(messages, resp.AsMessage())
		accounted = len(messages)

		if len(resp.ToolCalls) == 0 {
			return messages, nil
		}

		fps := make([]string, 0, len(resp.ToolCalls))
		for _, c := range resp.ToolCalls {
			fps = append(fps, fingerprint(c.Name, c.Arguments))
		}
		if detector.seen(fps) {
			repeated := repeatedCallNames(resp.ToolCalls)
			// Interactive posture: force the repeated call(s) through a human
			// with a "possible loop" preview rather than aborting. On approval
			// the run continues and the detector resets (another full window
			// before it re-prompts); on rejection it aborts. A nil hook is the
			// non-interactive posture — abort immediately. See change 0038.
			if a.LoopApproval != nil {
				preview := fmt.Sprintf("possible loop: %s repeated %d times — continue?", repeated, loopLimit)
				approved, err := a.LoopApproval(ctx, preview)
				if err != nil {
					return messages, err
				}
				if !approved {
					a.renderer.Errorf("aborting: %s repeated %d times (not approved)", repeated, loopLimit)
					return messages, fmt.Errorf("%w: %s repeated %d times", ErrLoopDetected, repeated, loopLimit)
				}
				detector.reset()
			} else {
				a.renderer.Errorf("aborting: %s repeated %d times", repeated, loopLimit)
				return messages, fmt.Errorf("%w: %s repeated %d times", ErrLoopDetected, repeated, loopLimit)
			}
		}

		toolMsgs := a.executeTools(ctx, resp.ToolCalls)
		messages = append(messages, toolMsgs...)
	}
	return messages, ErrMaxTurns
}

// repeatedCallNames renders the tool-call set for a doom-loop preview/abort
// message: the distinct call names in order, e.g. "bash" or "bash, read_file".
func repeatedCallNames(calls []model.ToolCall) string {
	seen := make(map[string]bool, len(calls))
	names := make([]string, 0, len(calls))
	for _, c := range calls {
		if !seen[c.Name] {
			seen[c.Name] = true
			names = append(names, c.Name)
		}
	}
	return strings.Join(names, ", ")
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
		// Output is already spill-bounded by the tool registry; history and
		// display see the same recoverable form.
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
