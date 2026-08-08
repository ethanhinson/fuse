package agent

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/ethanhinson/fuse/internal/model"
)

// classifierResultPrefixBytes caps the tool-result body sent to the classifier
// per candidate, bounding the request size. Internal constant, not config.
const classifierResultPrefixBytes = 512

// classifierPrompt instructs the model to score each candidate's relevance to
// the current task in [0,1], one score per line, no prose.
const classifierPrompt = `You rank tool results by how important each is to keep in an agent's context for the CURRENT task.
For EACH candidate below, output one line: its index and a relevance score in [0,1] (1 = must keep, 0 = safe to drop).
Format each line exactly as "N: 0.NN" (the candidate index, a colon, then the score). Output only these lines, no prose, no tools.`

// classifierScorer wraps the heuristic scorer with an optional LLM refinement of
// the borderline band (change 0028). The heuristic ranks every candidate first;
// only results whose heuristic score falls in [lo, hi] are batched to the
// classifier. Clear-cut results skip the model call. Any classifier failure,
// timeout, or parse error falls back to the heuristic score for the affected
// items — never worse than the heuristic ranking, which is never worse than
// recency.
//
// The classifier reuses the bounded transport (Completer, typically a
// *model.Adapter decorated WithTraceLabel(..., "relevance-classifier")) so
// per-attempt timeout, response-header timeout, bounded retries, and a distinct
// trace label all apply — the bound-every-model-call learning.
type classifierScorer struct {
	heuristic *heuristicScorer
	completer Completer
	modelID   string
	batchSize int
	lo, hi    float64

	// suppressed, once armed after a classifier failure, makes every subsequent
	// Score call skip the model and use the heuristic — so a persistently
	// failing classifier cannot hot-loop across successive prunes in one run.
	suppressed bool
}

// newClassifierScorer builds a hybrid scorer over a bounded Completer. batchSize
// <= 0 falls back to 1; an inverted or degenerate [lo,hi] band simply classifies
// nothing (every result is "clear-cut"), which is a safe no-op.
func newClassifierScorer(h *heuristicScorer, c Completer, modelID string, batchSize int, lo, hi float64) *classifierScorer {
	if batchSize <= 0 {
		batchSize = 1
	}
	return &classifierScorer{heuristic: h, completer: c, modelID: modelID, batchSize: batchSize, lo: lo, hi: hi}
}

// Score returns the heuristic score for a clear-cut result, or a classifier-
// refined score for a borderline result. Because RelevanceScorer.Score is
// per-result but the classifier batches, a borderline result triggers a single
// classifier call for JUST that result (batching across a prune pass is handled
// by ScoreBatch below; Score satisfies the interface and is used where a scorer
// is consulted one result at a time). Any failure returns the heuristic score.
func (cs *classifierScorer) Score(r ToolResult, ctx ScoreContext) float64 {
	h := cs.heuristic.Score(r, ctx)
	if cs.suppressed || !cs.borderline(h) {
		return h
	}
	scores, ok := cs.classify(context.Background(), []ToolResult{r}, ctx)
	if !ok || len(scores) != 1 {
		cs.suppressed = true
		return h
	}
	return scores[0]
}

// borderline reports whether a heuristic score falls in the classify band.
func (cs *classifierScorer) borderline(h float64) bool {
	return h >= cs.lo && h <= cs.hi && cs.lo < cs.hi
}

// classify sends a batch of candidates to the model and parses one [0,1] score
// per candidate, in input order. Returns (scores, false) on any transport error,
// empty response, or a response that does not yield a score for every candidate
// — the caller then falls back to the heuristic. Never propagates the error.
func (cs *classifierScorer) classify(ctx context.Context, batch []ToolResult, sctx ScoreContext) ([]float64, bool) {
	if len(batch) == 0 {
		return nil, false
	}
	req := model.CompletionReq{
		Model:      cs.modelID,
		Messages:   cs.buildMessages(batch, sctx),
		ToolChoice: "none",
		MaxTokens:  256,
	}
	resp, err := cs.completer.Complete(ctx, req)
	if err != nil {
		return nil, false
	}
	scores, ok := parseClassifierScores(resp.Content, len(batch))
	if !ok {
		return nil, false
	}
	return scores, true
}

// buildMessages renders the classifier request: the prompt, the current query,
// and each candidate as an indexed (toolName, args, capped result prefix) block.
func (cs *classifierScorer) buildMessages(batch []ToolResult, sctx ScoreContext) []model.Message {
	var b strings.Builder
	b.WriteString("Current task: ")
	b.WriteString(strings.TrimSpace(sctx.Query))
	b.WriteString("\n\nCandidates:\n")
	for i, r := range batch {
		body := r.Result
		if len(body) > classifierResultPrefixBytes {
			body = body[:classifierResultPrefixBytes]
		}
		fmt.Fprintf(&b, "%d: tool=%s args=%s result=%s\n", i, r.ToolName, oneLine(r.Args), oneLine(body))
	}
	return []model.Message{
		{Role: "system", Content: classifierPrompt},
		{Role: "user", Content: b.String()},
	}
}

// oneLine collapses whitespace so a candidate block stays on one line.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// parseClassifierScores parses the model's "N: score" lines into a dense slice
// of length want, in index order. It is defensive: it accepts lines in any
// order, tolerates surrounding prose, clamps scores to [0,1], and requires a
// score for every index 0..want-1 (otherwise ok=false so the caller falls back
// to the heuristic for the whole batch).
func parseClassifierScores(out string, want int) ([]float64, bool) {
	got := make([]float64, want)
	seen := make([]bool, want)
	for _, line := range strings.Split(out, "\n") {
		colon := strings.Index(line, ":")
		if colon < 0 {
			continue
		}
		idxStr := strings.TrimSpace(line[:colon])
		// Strip any leading prose before the number.
		if f := strings.Fields(idxStr); len(f) > 0 {
			idxStr = f[len(f)-1]
		}
		idx, err := strconv.Atoi(idxStr)
		if err != nil || idx < 0 || idx >= want {
			continue
		}
		scoreStr := strings.TrimSpace(line[colon+1:])
		if f := strings.Fields(scoreStr); len(f) > 0 {
			scoreStr = f[0]
		}
		v, err := strconv.ParseFloat(scoreStr, 64)
		if err != nil {
			continue
		}
		got[idx] = clamp01(v)
		seen[idx] = true
	}
	for _, s := range seen {
		if !s {
			return nil, false
		}
	}
	return got, true
}
