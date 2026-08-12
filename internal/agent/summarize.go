package agent

import (
	"context"
	"strings"

	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/observe"
)

// summarizerInputBudgetTokens caps the estimated input the summarizer will send
// in one call (candidate region + prompt scaffold + previous summary). It is an
// internal constant, not config: the input ladder (D2) shrinks the candidate
// region deterministically to fit under it before giving up. Sized well under a
// typical context window so the summarizer call itself never overflows.
const summarizerInputBudgetTokens = 96_000

// ladderRung reports which input-ladder rung produced a summary, for tests and
// diagnostics.
type ladderRung int

const (
	rungFull         ladderRung = iota // whole region fit
	rungDropOldest                     // oldest turns dropped
	rungStripOutputs                   // tool outputs stripped
	rungExhausted                      // nothing fit — fall back to Tier-1
)

// summarizerPrompt is the ODSNF instruction (design doc). The summarizer emits
// a single structured document; the previous summary, when present, is folded
// in with an update-in-place instruction (D3, anchoring).
const summarizerPrompt = `You are compacting an agent's tool-result history to free context.
Produce ONE structured summary with exactly these sections, preserving intent:

Objective: what the agent was trying to do
Details:   key findings and intermediate results
State:     current progress and open items
Next:      planned next steps
Files:     files touched and their state

Be concise but keep information the agent needs to continue without re-running tools.
Reply with the summary text only — do not call any tools.`

const anchorInstruction = `A previous summary exists below. UPDATE it in place — carry its still-relevant
content forward and fold in the new tool results — rather than starting over. Emit one merged summary.`

// summaryHeader labels the injected summary so the model recognizes it as a
// compaction artifact rather than fresh tool output.
const summaryHeader = "[context compacted — the older tool results below were summarized to free context]"

// buildSummaryMessage assembles the synthetic message injected at the
// protected-region boundary in place of the raw tool-result span. It is an
// assistant text message with NO tool_calls: pruneOldToolResults only touches
// tool-role messages, so the summary is never re-stubbed, and carrying no
// tool_call means it can never orphan a tool-result pair.
//
// The recovery pointer line ("grep your past at <path>") appears ONLY when
// pointer is non-empty — i.e. a real SegmentSink persisted the raw region
// (#0030). With the default no-op sink the pointer is empty and the line is
// omitted (D1).
func buildSummaryMessage(summary, pointer string) model.Message {
	var b strings.Builder
	b.WriteString(summaryHeader)
	b.WriteString("\n\n")
	b.WriteString(strings.TrimSpace(summary))
	if strings.TrimSpace(pointer) != "" {
		b.WriteString("\n\n")
		b.WriteString("Recovery: grep your past at ")
		b.WriteString(pointer)
	}
	return model.Message{Role: "assistant", Content: b.String()}
}

// summarizer runs a bounded LLM summarization pass over an old tool-result
// region. It reuses the bounded transport (Completer, typically a
// *model.Adapter decorated WithTraceLabel(..., "summarizer")) so per-attempt
// timeout, response-header timeout, bounded retries, and a distinct trace label
// all apply — the bound-every-model-call learning.
type summarizer struct {
	completer Completer
	modelID   string
	maxOutput int
	// inputBudgetTokens bounds the estimated input for one call; the ladder
	// shrinks the region to fit under it. Defaults to
	// summarizerInputBudgetTokens; overridable in tests.
	inputBudgetTokens int
	observer          observe.Observer
}

// newSummarizer builds a summarizer over a bounded Completer, the resolved
// model id, and the configured output cap.
func newSummarizer(c Completer, modelID string, maxOutput int) *summarizer {
	return &summarizer{
		completer:         c,
		modelID:           modelID,
		maxOutput:         maxOutput,
		inputBudgetTokens: summarizerInputBudgetTokens,
		observer:          observe.NoopObserver{},
	}
}

func (s *summarizer) setObserver(o observe.Observer) {
	if o == nil {
		o = observe.NoopObserver{}
	}
	s.observer = o
}

// summarize compacts region into a single ODSNF summary, folding previousSummary
// in when non-empty (anchoring). It returns ("", false) on any failure —
// transport error, empty output, or ladder exhaustion — so the caller falls
// back to Tier-1 pruning. It never propagates the model error.
func (s *summarizer) summarize(ctx context.Context, region []model.Message, previousSummary string) (string, bool) {
	out, ok, _ := s.summarizeWithRung(ctx, region, previousSummary)
	return out, ok
}

// summarizeWithRung is summarize plus the ladder rung that produced the result,
// exposed for tests.
func (s *summarizer) summarizeWithRung(ctx context.Context, region []model.Message, previousSummary string) (string, bool, ladderRung) {
	// Pre-flight input ladder (D2): shrink the candidate region deterministically
	// until its estimated input fits the summarizer's own budget, then call once.
	fitted, rung := s.fitRegion(region, previousSummary)
	if rung == rungExhausted {
		return "", false, rungExhausted
	}

	req := model.CompletionReq{
		Model:      s.modelID,
		Messages:   s.buildMessages(fitted, previousSummary),
		ToolChoice: "none", // force a text-only reply — the summarizer wants no tools
		MaxTokens:  s.maxOutput,
	}
	callCtx, span := s.observer.Start(ctx, observe.Descriptor{Kind: observe.OperationModelAttempt, Name: "complete", Fields: []observe.Field{{Key: "model", Value: s.modelID}}})
	resp, err := s.completer.Complete(callCtx, req)
	span.End(modelOutcome(err))
	if err != nil {
		return "", false, rung
	}
	out := strings.TrimSpace(resp.Content)
	if out == "" {
		return "", false, rung
	}
	return out, true, rung
}

// buildMessages renders the summarizer request: a system-role instruction (with
// the anchor note when a previous summary is carried), the previous summary (if
// any), and the candidate region as-is (tool messages) so the model sees the
// real content it is compacting.
func (s *summarizer) buildMessages(region []model.Message, previousSummary string) []model.Message {
	prompt := summarizerPrompt
	msgs := make([]model.Message, 0, len(region)+2)
	if strings.TrimSpace(previousSummary) != "" {
		prompt = prompt + "\n\n" + anchorInstruction
		msgs = append(msgs, model.Message{Role: "system", Content: prompt})
		msgs = append(msgs, model.Message{Role: "user", Content: "PREVIOUS SUMMARY:\n" + previousSummary})
	} else {
		msgs = append(msgs, model.Message{Role: "system", Content: prompt})
	}
	// Re-render the region as plain user content so it is a text-only payload
	// (no tool_call/tool-result pairing on the summarizer request — the region's
	// original messages are tool-role replies without their originating
	// assistant tool_calls, which some providers reject).
	var b strings.Builder
	b.WriteString("TOOL RESULTS TO COMPACT:\n")
	for _, m := range region {
		if m.Name != "" {
			b.WriteString("[")
			b.WriteString(m.Name)
			b.WriteString("] ")
		}
		b.WriteString(m.Content)
		b.WriteString("\n")
	}
	msgs = append(msgs, model.Message{Role: "user", Content: b.String()})
	return msgs
}

// scaffoldTokens estimates the non-region overhead (prompt + optional anchor
// instruction + previous summary).
func (s *summarizer) scaffoldTokens(previousSummary string) int {
	overhead := len(summarizerPrompt) + len(previousSummary)
	if strings.TrimSpace(previousSummary) != "" {
		overhead += len(anchorInstruction)
	}
	return overhead / bytesPerToken
}

// fitRegion applies the input ladder: full → drop oldest turns → strip tool
// outputs → exhausted. It returns the surviving region and the rung reached. A
// rungExhausted result means even a fully stripped region plus scaffold cannot
// fit the input budget, so the caller must fall back to Tier-1.
func (s *summarizer) fitRegion(region []model.Message, previousSummary string) ([]model.Message, ladderRung) {
	scaffold := s.scaffoldTokens(previousSummary)
	fits := func(msgs []model.Message) bool {
		return scaffold+messagesSize(msgs)/bytesPerToken <= s.inputBudgetTokens
	}

	if fits(region) {
		return region, rungFull
	}

	// Rung 1: drop oldest turns (oldest messages first) until it fits, keeping at
	// least the newest message.
	trimmed := append([]model.Message(nil), region...)
	for len(trimmed) > 1 {
		trimmed = trimmed[1:]
		if fits(trimmed) {
			return trimmed, rungDropOldest
		}
	}
	if fits(trimmed) {
		return trimmed, rungDropOldest
	}

	// Rung 2: strip tool outputs from the surviving region (replace bodies with
	// the stub) — keeps the tool names/shape but drops the bulk.
	stripped := make([]model.Message, len(trimmed))
	for i, m := range trimmed {
		m.Content = prunedStub
		stripped[i] = m
	}
	if fits(stripped) {
		return stripped, rungStripOutputs
	}

	// Rung 3: even the stub-only region plus scaffold overflows — give up.
	return nil, rungExhausted
}
