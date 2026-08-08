package agent

import (
	"sort"
	"strings"

	"github.com/ethanhinson/fuse/internal/model"
)

// RelevanceScorer ranks a tool result's importance to the current task in
// [0,1]. Higher = more worth protecting from pruning. Implementations must be
// deterministic given identical inputs. See change 0028.
type RelevanceScorer interface {
	Score(r ToolResult, ctx ScoreContext) float64
}

// ToolResult is the scorable unit — one "tool" role message plus its origin
// turn and the arguments of the assistant call that produced it.
type ToolResult struct {
	ToolName string
	Args     string
	Result   string
	Turn     int
}

// ScoreContext is the current-conversation signal the scorer ranks against.
type ScoreContext struct {
	Query      string   // the current (latest) user message
	RecentArgs []string // args of recent assistant tool calls (dependency signal)
	CurTurn    int      // the latest turn index, for recency decay
}

// --- tokenizer (shared by overlap + dependency-reuse signals) ---

// stopwords is a small set of common tokens dropped from the tokenizer output
// so they neither inflate overlap nor register as dependency tokens.
var stopwords = map[string]bool{
	"the": true, "and": true, "for": true, "you": true, "are": true,
	"was": true, "with": true, "this": true, "that": true, "from": true,
	"not": true, "but": true, "have": true, "has": true, "can": true,
	"all": true, "any": true, "out": true, "use": true, "get": true,
}

// tokenize splits s on any run of characters outside [A-Za-z0-9_/.:-],
// lowercases, and drops tokens shorter than 3 chars and the small stopword set.
// Deterministic and allocation-bounded by the input length.
func tokenize(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		switch {
		case r >= 'a' && r <= 'z':
			return false
		case r >= 'A' && r <= 'Z':
			return false
		case r >= '0' && r <= '9':
			return false
		case r == '_' || r == '/' || r == '.' || r == ':' || r == '-':
			return false
		default:
			return true
		}
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		t := strings.ToLower(f)
		if len(t) < 3 || stopwords[t] {
			continue
		}
		out = append(out, t)
	}
	return out
}

// distinctive reports whether a token is path-like or identifier-like — the
// kind of token whose reappearance in a later tool call signals dependency
// reuse. Path/line-ref tokens contain '/', '.', or ':'; identifier-like tokens
// contain '_' or mix letters with digits.
func distinctive(tok string) bool {
	if strings.ContainsAny(tok, "/.:") {
		return true
	}
	if strings.Contains(tok, "_") {
		return true
	}
	hasLetter, hasDigit := false, false
	for _, r := range tok {
		if r >= 'a' && r <= 'z' {
			hasLetter = true
		}
		if r >= '0' && r <= '9' {
			hasDigit = true
		}
	}
	return hasLetter && hasDigit
}

// --- heuristic scorer ---

// Heuristic weight constants. The weighted sum is clamped to [0,1]. Recency is
// top-weighted — it is the signal that makes the no-op degeneration invariant
// hold and the final tiebreaker. Tunable in-code; exercised by unit tests.
const (
	weightToolType = 0.15
	weightOverlap  = 0.25
	weightSignal   = 0.10
	weightDepReuse = 0.15
	weightRecency  = 0.35
)

// toolTypeBaseWeight is the static tool-type importance table, normalized to
// [0,1]. Unknown tools get the middle default.
func toolTypeBaseWeight(name string) float64 {
	switch name {
	case "read_file":
		return 1.0
	case "grep", "search":
		return 0.8
	case "bash":
		return 0.6
	case "list_directory":
		return 0.3
	default:
		return 0.6
	}
}

// signalKeywords trigger the failure/flagged-code boost.
var signalKeywords = []string{"error", "panic", "failed", "failure", "todo", "fixme", "exception"}

// heuristicScorer is the always-on, pure, deterministic default scorer. When
// recencyOnly is true every non-recency weight is suppressed, yielding the
// pure-recency degeneration path (context.relevance.heuristic: false).
type heuristicScorer struct {
	bodyScanBytes int
	recencyOnly   bool
}

// defaultHeuristicScorer builds the heuristic scorer with the spec-default
// body-scan cap. Used by New() so the prune step never sees a nil scorer.
func defaultHeuristicScorer() *heuristicScorer {
	return &heuristicScorer{bodyScanBytes: 2048}
}

// recencyOnlyScorer builds the pure-recency degeneration scorer.
func recencyOnlyScorer() *heuristicScorer {
	return &heuristicScorer{bodyScanBytes: 2048, recencyOnly: true}
}

// recencyComponent is the normalized recency score in [0,1]: newest (Turn ==
// CurTurn) scores 1.0, decaying linearly with age. A zero/negative span yields
// 1.0 so a single-turn history never penalizes.
func recencyComponent(r ToolResult, ctx ScoreContext) float64 {
	span := ctx.CurTurn
	if span <= 0 {
		return 1.0
	}
	age := ctx.CurTurn - r.Turn
	if age < 0 {
		age = 0
	}
	if age > span {
		age = span
	}
	return 1.0 - float64(age)/float64(span)
}

func (h *heuristicScorer) Score(r ToolResult, ctx ScoreContext) float64 {
	recency := recencyComponent(r, ctx)
	if h.recencyOnly {
		return clamp01(recency)
	}

	// Capped body prefix — bound the scan cost.
	body := r.Result
	if h.bodyScanBytes > 0 && len(body) > h.bodyScanBytes {
		body = body[:h.bodyScanBytes]
	}

	// Single tokenization pass over args + capped body feeds overlap (signal 2)
	// and dependency-token capture (signal 4).
	resultTokens := tokenize(r.Args + " " + body)
	resultSet := make(map[string]bool, len(resultTokens))
	for _, t := range resultTokens {
		resultSet[t] = true
	}

	// Signal 1 — tool-type base weight.
	toolType := toolTypeBaseWeight(r.ToolName)

	// Signal 2 — keyword overlap of the query + recent args against this result.
	ctxTokens := tokenize(ctx.Query)
	for _, a := range ctx.RecentArgs {
		ctxTokens = append(ctxTokens, tokenize(a)...)
	}
	overlap := 0.0
	if len(ctxTokens) > 0 {
		hits, seen := 0, map[string]bool{}
		for _, t := range ctxTokens {
			if seen[t] {
				continue
			}
			seen[t] = true
			if resultSet[t] {
				hits++
			}
		}
		overlap = float64(hits) / float64(len(seen))
	}

	// Signal 3 — signal-keyword boost (bounded to [0,1]).
	signal := 0.0
	lowerBody := strings.ToLower(body)
	for _, kw := range signalKeywords {
		if strings.Contains(lowerBody, kw) {
			signal = 1.0
			break
		}
	}

	// Signal 4 — bounded dependency reuse: a distinctive token of this result
	// reappears in a later tool call's args (RecentArgs). One pass; no graph.
	depReuse := 0.0
	if len(ctx.RecentArgs) > 0 {
		recentSet := map[string]bool{}
		for _, a := range ctx.RecentArgs {
			for _, t := range tokenize(a) {
				recentSet[t] = true
			}
		}
		for _, t := range resultTokens {
			if distinctive(t) && recentSet[t] {
				depReuse = 1.0
				break
			}
		}
	}

	sum := weightToolType*toolType +
		weightOverlap*overlap +
		weightSignal*signal +
		weightDepReuse*depReuse +
		weightRecency*recency
	return clamp01(sum)
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// --- prune selection helpers (used by pruneOldToolResults) ---

// scoredCandidate pairs a message index with its score and turn for the
// relevance fill's deterministic sort.
type scoredCandidate struct {
	idx   int
	score float64
	turn  int
	tok   int
}

// sortByScoreDescRecencyDesc orders candidates by score descending, then turn
// (recency) descending as the tiebreaker, then index descending for total
// determinism. Sort is stable-independent because the key is total.
func sortByScoreDescRecencyDesc(cs []scoredCandidate) {
	sort.Slice(cs, func(i, j int) bool {
		if cs[i].score != cs[j].score {
			return cs[i].score > cs[j].score
		}
		if cs[i].turn != cs[j].turn {
			return cs[i].turn > cs[j].turn
		}
		return cs[i].idx > cs[j].idx
	})
}

// --- ScoreContext + per-message derivation from history ---

// recentArgsTurns is how many of the most recent turns contribute their
// assistant tool-call args to ScoreContext.RecentArgs (the dependency signal).
const recentArgsTurns = 2

// deriveScoreContext builds the ScoreContext for a message history: the latest
// user message is the query, CurTurn is the highest turn index, and RecentArgs
// collects the assistant tool-call arguments from the most recent
// recentArgsTurns turns (newest-first). All derived from the slice itself — the
// loop's turn counter is never threaded through.
func deriveScoreContext(messages []model.Message) ScoreContext {
	turnOf := turnIndices(messages)
	curTurn := 0
	query := ""
	for i, m := range messages {
		if turnOf[i] > curTurn {
			curTurn = turnOf[i]
		}
		if m.Role == "user" {
			query = m.Content
		}
	}

	// RecentArgs: assistant tool-call args from turns in [curTurn-recentArgsTurns+1, curTurn],
	// newest-first (walk the history in reverse).
	minTurn := curTurn - recentArgsTurns + 1
	var recent []string
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if m.Role != "assistant" || len(m.ToolCalls) == 0 {
			continue
		}
		if turnOf[i] < minTurn {
			continue
		}
		for _, tc := range m.ToolCalls {
			if tc.Arguments != "" {
				recent = append(recent, tc.Arguments)
			}
		}
	}
	return ScoreContext{Query: query, RecentArgs: recent, CurTurn: curTurn}
}

// turnIndices assigns each message a 0-based turn number: the count of "user"
// messages at or before its index. Messages before the first user message are
// turn 0.
func turnIndices(messages []model.Message) []int {
	out := make([]int, len(messages))
	turn := 0
	for i, m := range messages {
		if m.Role == "user" {
			turn++
		}
		// A user message belongs to the turn it opens; use turn-1 as 0-based so
		// the first user message is turn 0 and its tool results share it.
		t := turn - 1
		if t < 0 {
			t = 0
		}
		out[i] = t
	}
	return out
}

// toolResultAt builds the ToolResult for the tool message at index i, pairing
// its ToolCallID to the arguments of the assistant call that produced it. An
// unpaired tool message (older histories) gets Args == "".
func toolResultAt(messages []model.Message, i int, argsByID map[string]string, turnOf []int) ToolResult {
	m := messages[i]
	return ToolResult{
		ToolName: m.Name,
		Args:     argsByID[m.ToolCallID],
		Result:   m.Content,
		Turn:     turnOf[i],
	}
}

// argsByToolCallID maps every assistant tool-call ID to its arguments in a
// single forward pass, so a tool result can recover the args that produced it.
func argsByToolCallID(messages []model.Message) map[string]string {
	out := map[string]string{}
	for _, m := range messages {
		if m.Role != "assistant" {
			continue
		}
		for _, tc := range m.ToolCalls {
			out[tc.ID] = tc.Arguments
		}
	}
	return out
}
