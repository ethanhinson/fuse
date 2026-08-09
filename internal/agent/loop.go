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
	// summarizeSuppressTurns is how many subsequent over-budget turns skip the
	// summarizer after a failure, so a persistently failing summarizer cannot
	// hot-loop a model call every turn (D2). Internal constant, not config —
	// mirrors pruneThresholdPct/pruneProtectTokens.
	summarizeSuppressTurns = 5
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

// pruneOldToolResults stubs tool results outside the protected budget and
// returns the estimated tokens freed. Only "tool" role messages are touched —
// user intent and assistant tool_calls stay intact, so provider tool pairing
// remains valid.
//
// Relevance-aware selection (change 0028): the protection budget is spent in
// two phases. A guaranteed recency floor (floorPct of protectTokens) protects
// the newest results exactly as before; the remaining budget is then filled by
// the highest-relevance results across all ages (scored by scorer, recency as
// the tiebreaker). Everything not protected by either phase is stubbed.
//
// No-op degeneration invariant: with a pure-recency scorer, phase 2 fills the
// remaining budget newest-first, so the protected set is byte-identical to the
// pre-0028 recency-only walk.
func pruneOldToolResults(messages []model.Message, protectTokens int, scorer RelevanceScorer, floorPct int) int {
	if scorer == nil {
		scorer = defaultHeuristicScorer()
	}
	if floorPct < 0 {
		floorPct = 0
	}
	if floorPct > 100 {
		floorPct = 100
	}

	// Collect candidate tool results (role == "tool", not already stubbed),
	// oldest→newest, with their estimated token cost.
	type cand struct {
		idx int
		tok int
	}
	var cands []cand
	for i := 0; i < len(messages); i++ {
		m := messages[i]
		if m.Role != "tool" || m.Content == prunedStub {
			continue
		}
		cands = append(cands, cand{idx: i, tok: len(m.Content) / bytesPerToken})
	}
	if len(cands) == 0 {
		return 0
	}

	protected := make(map[int]bool, len(cands))

	// Phase 1 — recency floor: protect the newest results up to
	// floor = protectTokens * floorPct / 100 (newest-first). The <floor test
	// mirrors the pre-0028 "seen < protectTokens" boundary so the newest result
	// straddling the floor stays protected.
	floor := protectTokens * floorPct / 100
	floorSpent := 0
	for i := len(cands) - 1; i >= 0; i-- {
		if floorSpent >= floor {
			break
		}
		protected[cands[i].idx] = true
		floorSpent += cands[i].tok
	}

	// Phase 2 — relevance fill: score the remaining candidates and protect the
	// highest-scoring (recency tiebreaker) until the remaining budget is spent.
	remaining := protectTokens - floorSpent
	if remaining > 0 {
		ctx := deriveScoreContext(messages)
		turnOf := turnIndices(messages)
		argsByID := argsByToolCallID(messages)
		var scored []scoredCandidate
		for _, c := range cands {
			if protected[c.idx] {
				continue
			}
			r := toolResultAt(messages, c.idx, argsByID, turnOf)
			scored = append(scored, scoredCandidate{
				idx:   c.idx,
				score: scorer.Score(r, ctx),
				turn:  turnOf[c.idx],
				tok:   c.tok,
			})
		}
		sortByScoreDescRecencyDesc(scored)
		spent := 0
		for _, s := range scored {
			if spent >= remaining {
				break
			}
			protected[s.idx] = true
			spent += s.tok
		}
	}

	// Stub every candidate not protected by either phase.
	freed := 0
	for _, c := range cands {
		if protected[c.idx] {
			continue
		}
		freed += c.tok
		messages[c.idx].Content = prunedStub
	}
	return freed
}

// summarizationRegion identifies the candidate span for Tier 2 summarization:
// the tool-result messages OLDER than the recency-protected tail (the same span
// pruneOldToolResults would stub, using the same protectTokens budget). It
// returns a copy of those messages, the insert index where the summary message
// should go (just before the protected region begins, at a turn boundary), the
// distinct tool names in order, and the estimated tokens the region occupies.
//
// The region is a fresh slice (safe to hand to a SegmentSink); already-stubbed
// tool results are skipped so a re-triggered compaction never re-summarizes
// stubs. Returns ok=false when there is nothing worth summarizing.
//
// firstIdx/lastIdx are the region's first and last ORIGINAL indices in messages
// (the min/max message indices of the compacted span), so the caller can derive
// the inclusive turn range via turnIndices (#0030 DRIFT 1).
func summarizationRegion(messages []model.Message, protectTokens int) (region []model.Message, insertAt int, toolNames []string, tokens, firstIdx, lastIdx int, ok bool) {
	// Walk from the end to find the protected/unprotected boundary by the same
	// recency rule pruneOldToolResults uses.
	seen := 0
	firstUnprotected := -1 // index of the oldest-scanning unprotected tool result
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if m.Role != "tool" || m.Content == prunedStub {
			continue
		}
		tok := len(m.Content) / bytesPerToken
		if seen < protectTokens {
			seen += tok
			continue
		}
		firstUnprotected = i // keep moving; ends at the OLDEST unprotected result
	}
	if firstUnprotected == -1 {
		return nil, 0, nil, 0, 0, 0, false
	}

	// Collect the unprotected tool results (oldest→newest) and their span. The
	// insert index is just before the first protected message after the span —
	// i.e. right after the last unprotected tool result — so the summary sits at
	// the boundary of the protected tail.
	nameSeen := map[string]bool{}
	lastUnprotected := firstUnprotected
	firstRegionIdx := -1
	for i := firstUnprotected; i < len(messages); i++ {
		m := messages[i]
		if m.Role != "tool" || m.Content == prunedStub {
			continue
		}
		tok := len(m.Content) / bytesPerToken
		// A message is unprotected iff it is not within the protected tail. Recompute
		// membership: everything from the tail inward up to protectTokens is protected.
		if isProtected(messages, i, protectTokens) {
			break
		}
		if firstRegionIdx == -1 {
			firstRegionIdx = i
		}
		region = append(region, m)
		tokens += tok
		lastUnprotected = i
		if m.Name != "" && !nameSeen[m.Name] {
			nameSeen[m.Name] = true
			toolNames = append(toolNames, m.Name)
		}
	}
	if len(region) == 0 {
		return nil, 0, nil, 0, 0, 0, false
	}
	return region, lastUnprotected + 1, toolNames, tokens, firstRegionIdx, lastUnprotected, true
}

// dropPriorSummary removes a previously-injected summary message (identified by
// the summaryHeader prefix) from messages, returning the compacted slice and the
// accounted/insertAt indices adjusted for the removal. Anchoring keeps exactly
// one living summary document: the new summary folds the old one in, so the old
// message is dropped before the new one is inserted. Only the first match is
// removed (there is never more than one). A prior summary is an assistant text
// message with no tool_calls, so removing it never orphans a tool pair.
func dropPriorSummary(messages []model.Message, accounted, insertAt int) ([]model.Message, int, int) {
	idx := -1
	for i, m := range messages {
		if m.Role == "assistant" && len(m.ToolCalls) == 0 && strings.HasPrefix(m.Content, summaryHeader) {
			idx = i
			break
		}
	}
	if idx == -1 {
		return messages, accounted, insertAt
	}
	out := make([]model.Message, 0, len(messages)-1)
	out = append(out, messages[:idx]...)
	out = append(out, messages[idx+1:]...)
	if idx < accounted {
		accounted--
	}
	if idx < insertAt {
		insertAt--
	}
	return out, accounted, insertAt
}

// insertMessage returns messages with m spliced in at index at (0 ≤ at ≤ len),
// allocating a fresh slice so callers never alias the original backing array.
func insertMessage(messages []model.Message, at int, m model.Message) []model.Message {
	if at < 0 {
		at = 0
	}
	if at > len(messages) {
		at = len(messages)
	}
	out := make([]model.Message, 0, len(messages)+1)
	out = append(out, messages[:at]...)
	out = append(out, m)
	out = append(out, messages[at:]...)
	return out
}

// isProtected reports whether the tool result at index i falls within the
// recency-protected tail (the newest protectTokens of tool results).
func isProtected(messages []model.Message, i, protectTokens int) bool {
	seen := 0
	for j := len(messages) - 1; j >= 0; j-- {
		m := messages[j]
		if m.Role != "tool" || m.Content == prunedStub {
			continue
		}
		tok := len(m.Content) / bytesPerToken
		if seen < protectTokens {
			if j == i {
				return true
			}
			seen += tok
			continue
		}
		if j == i {
			return false
		}
	}
	return false
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

	// Tier 2 summarization state (change 0027), loop-scoped so concurrent agents
	// never share it: previousSummary is the single living ODSNF document carried
	// forward (D3, anchoring); suppressUntil is the turn index through which the
	// summarizer is skipped after a failure (D2).
	previousSummary := ""
	suppressUntil := 0

	// a.maxTurns <= 0 means unlimited: the loop runs until the model stops
	// calling tools, context is exhausted, or the doom-loop detector trips.
	// A positive maxTurns caps the run and returns ErrMaxTurns. See change 0038.
	for turn := 0; a.maxTurns <= 0 || turn < a.maxTurns; turn++ {
		if err := ctx.Err(); err != nil {
			return messages, err
		}

		// Turn boundary: drain any human messages queued for this node and inject
		// them as a single batched user turn before the model is called (ADR-0022).
		// This is a self-pull — the node reads its own queue at a point where it is
		// between model calls — so ADR-0016's run-to-completion contract holds.
		if hm, ok := a.humanInjector.Poll(); ok {
			messages = append(messages, hm)
		}

		estimate := lastUsage + messagesSize(messages[accounted:])/bytesPerToken
		if estimate > budget {
			// Tier 2 (change 0027): before the deterministic stub prune, run a
			// bounded LLM summarization pass over the old tool-result region and
			// inject a single anchored ODSNF summary at the protected-region
			// boundary. Fail-safe: any failure falls through to Tier-1 and arms a
			// bounded suppression window so a failing summarizer cannot hot-loop.
			if a.summarizer != nil && turn >= suppressUntil {
				protect := protectBudget(window, false)
				region, insertAt, toolNames, tokensBefore, firstIdx, lastIdx, ok := summarizationRegion(messages, protect)
				if ok {
					if summary, done := a.summarizer.summarize(ctx, region, previousSummary); done {
						// Turn span of the compacted region, derived from the region's
						// original message indices (#0030 DRIFT 1).
						ti := turnIndices(messages)
						// Best-effort archive of the raw region (#0030 implements a real
						// sink; the default no-op returns "" so no recovery pointer).
						pointer, aerr := a.segmentSink.Archive(SegmentRegion{
							TurnStart:    ti[firstIdx],
							TurnEnd:      ti[lastIdx],
							Messages:     region,
							Summary:      summary,
							ToolNames:    toolNames,
							TokensBefore: tokensBefore,
							TokensAfter:  len(summary) / bytesPerToken,
						})
						if aerr != nil {
							a.renderer.Errorf("segment archive failed (non-fatal): %v", aerr)
							pointer = ""
						}
						// Anchoring (D3): the new summary folds the previous one in, so the
						// prior injected summary message is removed before the new one is
						// added — only ONE living summary document survives in context.
						messages, accounted, insertAt = dropPriorSummary(messages, accounted, insertAt)
						// Inject the summary at the boundary (assistant text message, no
						// tool_calls — never orphans a pair, never re-stubbed by prune).
						summaryMsg := buildSummaryMessage(summary, pointer)
						messages = insertMessage(messages, insertAt, summaryMsg)
						// Keep the hybrid-accounting boundary aligned: inserting before
						// `accounted` shifts every later index by one.
						if insertAt <= accounted {
							accounted++
						}
						previousSummary = summary
						// Recompute what the prune step will still need to do: the raw
						// region is stubbed below by the unchanged Tier-1 path.
						a.renderer.Errorf("context: ~%dk/%dk tokens — summarized ~%dk of old tool results",
							estimate/1000, window/1000, tokensBefore/1000)
						// Fall through to the Tier-1 prune, which stubs the now-summarized
						// raw region (keeping tool pairing valid) and recomputes below.
					} else {
						// Summarizer failed or ladder-exhausted: arm suppression.
						suppressUntil = turn + 1 + summarizeSuppressTurns
					}
				}
				// The summary message was appended after `accounted`, so the estimate
				// below re-includes it; the raw region is stubbed by the prune. Recompute.
				estimate = lastUsage + messagesSize(messages[accounted:])/bytesPerToken
			}

			freed := pruneOldToolResults(messages, protectBudget(window, false), a.relevanceScorer, a.recencyFloorPct())
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

		schemas := a.tools.Schemas()
		if a.stripSpawn != nil && a.stripSpawn() {
			schemas = withoutSpawnAgent(schemas)
		}
		req := model.CompletionReq{
			Model:     a.modelID,
			Messages:  a.withSystem(messages),
			Tools:     schemas,
			MaxTokens: a.maxTokens,
		}
		resp, err := a.model.Complete(ctx, req)
		if err != nil {
			// The estimate said we fit but the provider disagreed: prune hard
			// (deterministic — recovery must not depend on another LLM call)
			// and retry exactly once.
			if isContextLengthErr(err) && !retriedContext {
				retriedContext = true
				if freed := pruneOldToolResults(messages, protectBudget(window, true), a.relevanceScorer, a.recencyFloorPct()); freed > 0 {
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
				results[i] = toolResult{call: call, res: a.executeToolBounded(ctx, call)}
			}(i, call)
		}
		wg.Wait()
	} else {
		for i, call := range calls {
			a.renderer.ToolCall(call.Name, call.Arguments)
			results[i] = toolResult{call: call, res: a.executeToolBounded(ctx, call)}
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

// executeToolBounded runs one tool call under the per-tool-call timeout so a
// hung leaf tool (e.g. a bash command that never returns) cannot block the whole
// agent loop indefinitely. Orchestration tools (spawn_agent, pipeline_run) are
// exempt — they legitimately run long by awaiting child agents and are bounded
// by their own budgets and the parent context. On timeout the tool's derived
// context is cancelled (so a well-behaved tool unwinds) and an error Result is
// returned to the model describing what happened; the underlying goroutine is
// left to observe cancellation on its own rather than blocking the loop.
func (a *Agent) executeToolBounded(ctx context.Context, call model.ToolCall) tools.Result {
	if toolTimeoutExempt(call.Name) {
		return a.tools.Execute(ctx, call.Name, call.Arguments)
	}
	timeout := a.toolTimeout
	if timeout <= 0 {
		timeout = DefaultToolTimeout
	}
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	done := make(chan tools.Result, 1)
	go func() {
		done <- a.tools.Execute(tctx, call.Name, call.Arguments)
	}()

	select {
	case res := <-done:
		return res
	case <-tctx.Done():
		// Distinguish a per-tool timeout from overall cancellation so the model
		// gets an actionable message rather than a bare context error.
		if errors.Is(tctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			return tools.Result{
				IsError: true,
				Output: fmt.Sprintf("tool %q exceeded the %s per-call timeout and was abandoned; "+
					"try a narrower or faster invocation", call.Name, timeout),
			}
		}
		return tools.Result{IsError: true, Output: fmt.Sprintf("tool %q cancelled: %v", call.Name, tctx.Err())}
	}
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

// withoutSpawnAgent returns schemas with any "spawn_agent" entry removed,
// preserving order. It allocates a fresh slice so it never mutates the
// registry's backing array. Returns the input unchanged when spawn_agent is
// absent (the common stripped-child case).
func withoutSpawnAgent(schemas []model.ToolSchema) []model.ToolSchema {
	present := false
	for _, s := range schemas {
		if s.Name == "spawn_agent" {
			present = true
			break
		}
	}
	if !present {
		return schemas
	}
	out := make([]model.ToolSchema, 0, len(schemas)-1)
	for _, s := range schemas {
		if s.Name == "spawn_agent" {
			continue
		}
		out = append(out, s)
	}
	return out
}
