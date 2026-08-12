package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/observe"
	"github.com/ethanhinson/fuse/internal/permissions"
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

// returnResultCap bounds structured-delegation self-repair (change 0042 D4): up
// to this many non-conforming return_result calls are answered with the
// validation error and retried; the next one ends the run with no captured value
// (ErrNoStructuredResult at the spawner, never a hard failure). N=2 per spec.
const returnResultCap = 2

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
	// Capture only the repository-owned durable subset once for this operation.
	// Every emitted transition then carries the same causal context without pulling
	// a telemetry vendor into the agent package.
	a.eventTrace = observe.TraceCarrier(a.observer, ctx)
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

	// Structured-delegation state (change 0042): when expects is set, the loop
	// offers a synthesized return_result tool and treats a conforming call as
	// terminal. returnResultAttempts counts non-conforming return_result calls; on
	// the (N+1)th it stops being treated as retryable and the run ends with no
	// captured value (ErrNoStructuredResult posture at the spawner). See D3/D4.
	returnResultAttempts := 0

	// a.maxTurns <= 0 means unlimited: the loop runs until the model stops
	// calling tools, context is exhausted, or the doom-loop detector trips.
	// A positive maxTurns caps the run and returns ErrMaxTurns. See change 0038.
	for turn := 0; a.maxTurns <= 0 || turn < a.maxTurns; turn++ {
		if err := ctx.Err(); err != nil {
			a.emit(event.KindError, turn, event.ErrorPayload{Err: err.Error(), Turn: turn})
			return messages, err
		}
		// turn.start (change 0043): every turn boundary is a first-class event.
		a.emit(event.KindTurnStart, turn, event.TurnStartPayload{Turn: turn})

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
						// context.summarize (change 0043): emitted beside the segment
						// archive so a consumer sees the Tier-2 compaction as an event.
						a.emit(event.KindSummarize, turn, event.SummarizePayload{
							TurnStart:    ti[firstIdx],
							TurnEnd:      ti[lastIdx],
							ToolNames:    toolNames,
							TokensBefore: tokensBefore,
							TokensAfter:  len(summary) / bytesPerToken,
							Pointer:      pointer,
						})
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
				a.emit(event.KindError, turn, event.ErrorPayload{Err: err.Error(), Turn: turn})
				return messages, err
			}
		}

		schemas := a.tools.Schemas()
		if a.stripSpawn != nil && a.stripSpawn() {
			schemas = withoutSpawnAgent(schemas)
		}
		// Structured-delegation (change 0042): offer the synthesized return_result
		// tool alongside the registry's tools when expects is set, so the child
		// returns its verdict on the tool channel. Per-run only — a child with no
		// expects never sees it (the append is guarded on expectsSchema != nil).
		if a.expectsSchema != nil {
			schemas = withReturnResultTool(schemas, a.expectsSchema)
		}
		req := model.CompletionReq{
			Model:     a.modelID,
			Messages:  a.withSystem(messages),
			Tools:     schemas,
			MaxTokens: a.maxTokens,
		}
		// model.call.start (change 0043): the model-call boundary opens here.
		a.emit(event.KindModelCallStart, turn, event.ModelCallStartPayload{
			Model:    a.modelID,
			MsgCount: len(req.Messages),
		})
		modelCtx, modelSpan := a.observer.Start(ctx, observe.Descriptor{Kind: observe.OperationModelAttempt, Name: "complete", Fields: []observe.Field{{Key: "model", Value: a.modelID}}})
		resp, err := a.model.Complete(modelCtx, req)
		modelOutcome := observe.OutcomeSuccess
		if errors.Is(err, context.DeadlineExceeded) {
			modelOutcome = observe.OutcomeTimeout
		} else if errors.Is(err, context.Canceled) {
			modelOutcome = observe.OutcomeCanceled
		} else if err != nil {
			modelOutcome = observe.OutcomeError
		}
		modelSpan.End(modelOutcome)
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
			a.emit(event.KindError, turn, event.ErrorPayload{Err: err.Error(), Turn: turn})
			return messages, err
		}
		// model.call.end (change 0043): full assistant response + usage + tool refs.
		a.emit(event.KindModelCallEnd, turn, event.ModelCallEndPayload{
			Content:      resp.Content,
			InputTokens:  resp.InputTokens,
			OutputTokens: resp.OutputTokens,
			ToolCalls:    toolCallRefs(resp.ToolCalls),
		})
		a.renderer.Tokens(resp.InputTokens, resp.OutputTokens)
		lastUsage = resp.InputTokens + resp.OutputTokens

		if resp.Content != "" {
			a.renderer.Assistant(resp.Content)
		}
		messages = append(messages, resp.AsMessage())
		accounted = len(messages)

		if len(resp.ToolCalls) == 0 {
			// turn.end (change 0043): the no-tool-calls terminal path.
			a.emit(event.KindTurnEnd, turn, event.TurnEndPayload{Turn: turn})

			// Interactive (persistent conversational) mode: instead of ending the run,
			// PARK at this turn boundary awaiting the next human message, so one loop_id
			// carries a full multi-turn conversation with in-memory history preserved
			// across turns. The next iteration's top-of-loop Poll (ADR-0022 self-pull)
			// injects the awaited message as a user turn — history is server-authoritative
			// because `messages` (the whole transcript) is carried forward unchanged.
			//
			// The park ends the run ONLY on ctx cancellation (client disconnect / server
			// shutdown) or when there is no bus to await (Wait returns errNoBus) — both
			// fall through to the normal terminal return, so a non-interactive loop and a
			// bus-less interactive loop are byte-identical to the pre-change behavior.
			if a.interactive && a.humanInjector != nil {
				// Announce the park FIRST: a deterministic "exchange complete, awaiting
				// your next message" signal carrying the final answer, so a conversational
				// client renders the reply and re-enables input without guessing which
				// model.call.end was terminal.
				a.emit(event.KindLoopParked, turn, event.LoopParkedPayload{Turn: turn, Content: resp.Content})
				if werr := a.humanInjector.Wait(ctx); werr == nil {
					// A message is pending; loop back so the top-of-turn Poll injects it.
					// maxTurns is unlimited for an interactive loop (set by the binding),
					// so the turn counter never caps a live conversation.
					continue
				}
				// errNoBus or ctx cancellation: end the run like a normal terminal turn.
			}
			return messages, nil
		}

		// Structured-delegation interception (change 0042): when expects is set,
		// a return_result call is handled by the loop, not dispatched to the
		// registry. A conforming call is TERMINAL — capture the validated value and
		// end the run (no further assistant message required). A non-conforming call
		// returns the validation error as its tool result so the child self-repairs,
		// bounded by returnResultCap; on exhaustion the run ends with no capture
		// (ErrNoStructuredResult posture at the spawner). Any sibling tool calls in
		// the same response (e.g. a write_file the child issued alongside its verdict)
		// still execute normally — the two channels never compete.
		//
		// Doom-loop interaction: the identical-call detector (loopLimit=3) runs
		// BELOW this block, so a return_result-only response is intercepted here and
		// never reaches the detector — the return_result retry cap bounds repair
		// independently of the doom-loop detector. (Exhaustion here ends the run
		// before the detector could trip on identical retries.)
		if a.expectsSchema != nil {
			var rr *model.ToolCall
			for i := range resp.ToolCalls {
				if resp.ToolCalls[i].Name == returnResultToolName {
					rr = &resp.ToolCalls[i]
					break
				}
			}
			if rr != nil {
				parsed, verr := validateAgainstSchema(a.expectsSchema, rr.Arguments)
				if verr == nil {
					// Terminal: capture + emit a trivial tool result, run any siblings,
					// then end the run. The captured value flows to the spawner via the
					// sink; SpawnDone.Result stays the human-facing text (may be empty).
					a.expectsSink.set(parsed)
					msgs := a.executeReturnResultTurn(ctx, turn, messages, resp.ToolCalls, "result recorded")
					messages = append(messages, msgs...)
					// turn.end (change 0043): the structured-delegation terminal path
					// closes the turn just like every other terminal path.
					a.emit(event.KindTurnEnd, turn, event.TurnEndPayload{Turn: turn})
					return messages, nil
				}
				// Non-conforming: retry unless the cap is exhausted.
				if returnResultAttempts >= returnResultCap {
					// Exhausted: end the run with no captured value. The spawner sees
					// no structured result and falls back / returns ErrNoStructuredResult.
					a.renderer.Errorf("return_result: %d invalid attempts — giving up on a structured result", returnResultAttempts)
					msgs := a.executeReturnResultTurn(ctx, turn, messages, resp.ToolCalls,
						"return_result exhausted: unable to produce a conforming result after "+
							fmt.Sprintf("%d", returnResultAttempts+1)+" attempts")
					messages = append(messages, msgs...)
					a.emit(event.KindTurnEnd, turn, event.TurnEndPayload{Turn: turn})
					return messages, nil
				}
				returnResultAttempts++
				msgs := a.executeReturnResultTurn(ctx, turn, messages, resp.ToolCalls,
					"return_result arguments did not match the required schema: "+verr.Error()+
						" — fix the arguments and call return_result again.")
				messages = append(messages, msgs...)
				// turn.end (change 0043): the return_result retry path also closes its
				// turn before looping to the next one.
				a.emit(event.KindTurnEnd, turn, event.TurnEndPayload{Turn: turn})
				continue
			}
		}

		fps := make([]string, 0, len(resp.ToolCalls))
		for _, c := range resp.ToolCalls {
			fps = append(fps, fingerprint(c.Name, c.Arguments))
		}
		if detector.seen(fps) {
			// loop.detector.trip (change 0043): emitted whenever the doom-loop
			// detector fires, before the interactive/abort branch below.
			a.emit(event.KindLoopTrip, turn, event.LoopTripPayload{Turn: turn})
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
					a.emit(event.KindError, turn, event.ErrorPayload{Err: err.Error(), Turn: turn})
					return messages, err
				}
				if !approved {
					a.renderer.Errorf("aborting: %s repeated %d times (not approved)", repeated, loopLimit)
					lerr := fmt.Errorf("%w: %s repeated %d times", ErrLoopDetected, repeated, loopLimit)
					a.emit(event.KindError, turn, event.ErrorPayload{Err: lerr.Error(), Turn: turn})
					return messages, lerr
				}
				detector.reset()
			} else {
				a.renderer.Errorf("aborting: %s repeated %d times", repeated, loopLimit)
				lerr := fmt.Errorf("%w: %s repeated %d times", ErrLoopDetected, repeated, loopLimit)
				a.emit(event.KindError, turn, event.ErrorPayload{Err: lerr.Error(), Turn: turn})
				return messages, lerr
			}
		}

		toolMsgs := a.executeTools(ctx, turn, messages, resp.ToolCalls)
		messages = append(messages, toolMsgs...)
		// turn.end (change 0043): the tool-dispatch path completed this turn.
		a.emit(event.KindTurnEnd, turn, event.TurnEndPayload{Turn: turn})
	}
	// error (change 0043): the loop hit its max-turns cap.
	a.emit(event.KindError, a.maxTurns, event.ErrorPayload{Err: ErrMaxTurns.Error(), Turn: a.maxTurns})
	return messages, ErrMaxTurns
}

// toolCallRefs projects model tool calls to lightweight event refs (id+name) for
// the model.call.end payload.
func toolCallRefs(calls []model.ToolCall) []event.ToolCallRef {
	if len(calls) == 0 {
		return nil
	}
	refs := make([]event.ToolCallRef, 0, len(calls))
	for _, c := range calls {
		refs = append(refs, event.ToolCallRef{ID: c.ID, Name: c.Name})
	}
	return refs
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
func (a *Agent) executeTools(ctx context.Context, turn int, messages []model.Message, calls []model.ToolCall) []model.Message {
	results := make([]toolResult, len(calls))

	// tool.call (change 0043): emit one event per requested call, with full args,
	// before any dispatch — greppable, ordered, and independent of the parallel /
	// serial execution path below.
	for _, c := range calls {
		a.emit(event.KindToolCall, turn, event.ToolCallPayload{
			ID:   c.ID,
			Name: c.Name,
			Args: json.RawMessage(c.Arguments),
		})
	}

	// Carry the user's conversation turns to the auto-mode classifier via the
	// permissions ctx-carry seam, so the gate can plumb them into its verdict
	// call WITHOUT widening the ToolExecutor interface (which would create an
	// agent<->permissions import cycle). userTurns is defense-in-depth: it drops
	// tool results and assistant reasoning up front; the classifier re-filters.
	tctx := permissions.WithUserMessages(ctx, userTurns(messages))

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
				results[i] = toolResult{call: call, res: a.executeToolBounded(tctx, call)}
			}(i, call)
		}
		wg.Wait()
	} else {
		for i, call := range calls {
			a.renderer.ToolCall(call.Name, call.Arguments)
			results[i] = toolResult{call: call, res: a.executeToolBounded(tctx, call)}
		}
	}

	msgs := make([]model.Message, 0, len(calls))
	for _, r := range results {
		a.renderer.ToolResult(r.call.Name, r.res)
		// tool.result (change 0043): full result output + error flag, per call.
		a.emit(event.KindToolResult, turn, event.ToolResultPayload{
			ID:      r.call.ID,
			Name:    r.call.Name,
			Result:  r.res.Output,
			IsError: r.res.IsError,
		})
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

// userTurns filters a running conversation down to the user-authored turns
// (Role=="user"), dropping tool results, assistant reasoning, and the system
// prompt. It is defense-in-depth for the auto-mode classifier seam: the
// classifier itself re-filters (permissions.buildMessages drops non-user
// roles), but narrowing here keeps a malicious tool result or actor
// rationalization out of the ctx-carried slice in the first place. It returns
// nil when there are no user turns.
func userTurns(msgs []model.Message) []model.Message {
	var out []model.Message
	for _, m := range msgs {
		if m.Role == "user" {
			out = append(out, m)
		}
	}
	return out
}

// executeToolBounded runs one tool call under the per-tool-call timeout so a
// hung leaf tool (e.g. a bash command that never returns) cannot block the whole
// agent loop indefinitely. Orchestration tools (spawn_agent, pipeline_run) are
// exempt — they legitimately run long by awaiting child agents and are bounded
// by their own budgets and the parent context. On timeout the tool's derived
// context is cancelled (so a well-behaved tool unwinds) and an error Result is
// returned to the model describing what happened; the underlying goroutine is
// left to observe cancellation on its own rather than blocking the loop.
func (a *Agent) executeToolBounded(ctx context.Context, call model.ToolCall) (result tools.Result) {
	ctx, span := a.observer.Start(ctx, observe.Descriptor{Kind: observe.OperationTool, Name: "execute", Fields: []observe.Field{{Key: "tool", Value: call.Name}}})
	out := observe.OutcomeSuccess
	defer func() {
		if errors.Is(ctx.Err(), context.Canceled) {
			out = observe.OutcomeCanceled
		} else if result.IsError && out == observe.OutcomeSuccess {
			out = observe.OutcomeError
		}
		span.End(out)
	}()
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
			out = observe.OutcomeTimeout
			return tools.Result{
				IsError: true,
				Output: fmt.Sprintf("tool %q exceeded the %s per-call timeout and was abandoned; "+
					"try a narrower or faster invocation", call.Name, timeout),
			}
		}
		out = observe.OutcomeCanceled
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

// withReturnResultTool returns schemas with the synthesized return_result tool
// (change 0042) appended — its Parameters are the expects schema. It allocates a
// fresh slice so it never mutates the registry's backing array. If a real tool
// named return_result already exists it is dropped in favor of the synthesized
// one (the Expects child's structured-output channel wins; documented in the
// spec — today none exists).
func withReturnResultTool(schemas []model.ToolSchema, expects map[string]any) []model.ToolSchema {
	out := make([]model.ToolSchema, 0, len(schemas)+1)
	for _, s := range schemas {
		if s.Name == returnResultToolName {
			continue
		}
		out = append(out, s)
	}
	out = append(out, model.ToolSchema{
		Name:        returnResultToolName,
		Description: returnResultDescription(),
		Parameters:  returnResultSchema(expects),
	})
	return out
}

// executeReturnResultTurn builds the tool-result messages for a response that
// carried a return_result call (change 0042). The return_result call itself is
// answered with rrOutput (a trivial "result recorded" on success, or the
// validation error on retry/exhaustion) WITHOUT dispatching it to the registry;
// any sibling calls in the same response are dispatched normally so the child's
// real work (e.g. a write_file issued alongside its verdict) still happens. The
// returned messages preserve the response's call order for valid tool pairing.
func (a *Agent) executeReturnResultTurn(ctx context.Context, turn int, messages []model.Message, calls []model.ToolCall, rrOutput string) []model.Message {
	// Dispatch the non-return_result siblings through the normal path.
	var siblings []model.ToolCall
	for _, c := range calls {
		if c.Name == returnResultToolName {
			continue
		}
		siblings = append(siblings, c)
	}
	sibResults := map[string]model.Message{}
	if len(siblings) > 0 {
		for _, m := range a.executeTools(ctx, turn, messages, siblings) {
			sibResults[m.ToolCallID] = m
		}
	}

	out := make([]model.Message, 0, len(calls))
	for _, c := range calls {
		if c.Name == returnResultToolName {
			a.renderer.ToolCall(c.Name, c.Arguments)
			res := tools.Result{Output: rrOutput}
			a.renderer.ToolResult(c.Name, res)
			// tool.call / tool.result (change 0043): the synthesized return_result
			// call is loop-handled (never dispatched to the registry), but it is still
			// a tool interaction the model performed — emit both so the event stream
			// is symmetric with normal tool calls. (Review finding.)
			a.emit(event.KindToolCall, turn, event.ToolCallPayload{
				ID:   c.ID,
				Name: c.Name,
				Args: json.RawMessage(c.Arguments),
			})
			a.emit(event.KindToolResult, turn, event.ToolResultPayload{
				ID:     c.ID,
				Name:   c.Name,
				Result: rrOutput,
			})
			out = append(out, model.Message{
				Role:       "tool",
				ToolCallID: c.ID,
				Name:       c.Name,
				Content:    rrOutput,
			})
			continue
		}
		if m, ok := sibResults[c.ID]; ok {
			out = append(out, m)
		}
	}
	return out
}
