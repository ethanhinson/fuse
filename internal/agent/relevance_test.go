package agent

import (
	"reflect"
	"testing"

	"github.com/ethanhinson/fuse/internal/model"
)

func TestTokenize(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"lowercases", "READ File", []string{"read", "file"}},
		{"drops short + stopwords", "the a go if internal/agent", []string{"internal/agent"}},
		{"keeps path chars", "internal/agent/loop.go:82", []string{"internal/agent/loop.go:82"}},
		{"splits on punctuation", "func(name, args)", []string{"func", "name", "args"}},
		{"empty", "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tokenize(tt.in)
			if len(got) == 0 && len(tt.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("tokenize(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestDistinctive(t *testing.T) {
	distinct := []string{"internal/agent/loop.go", "loop.go:82", "pkg:name", "my_var", "abc123"}
	plain := []string{"reading", "function", "value", "context"}
	for _, tok := range distinct {
		if !distinctive(tok) {
			t.Errorf("distinctive(%q) = false, want true", tok)
		}
	}
	for _, tok := range plain {
		if distinctive(tok) {
			t.Errorf("distinctive(%q) = true, want false", tok)
		}
	}
}

func TestHeuristicToolTypeOrdering(t *testing.T) {
	h := defaultHeuristicScorer()
	ctx := ScoreContext{CurTurn: 1}
	// Same turn, empty args/result: only the tool-type base differs.
	score := func(name string) float64 {
		return h.Score(ToolResult{ToolName: name, Turn: 1}, ctx)
	}
	if !(score("read_file") > score("grep") && score("grep") > score("bash") && score("bash") > score("list_directory")) {
		t.Errorf("tool-type ordering violated: read_file=%v grep=%v bash=%v list_directory=%v",
			score("read_file"), score("grep"), score("bash"), score("list_directory"))
	}
}

func TestHeuristicKeywordOverlapRaisesScore(t *testing.T) {
	h := defaultHeuristicScorer()
	// The query names the same path token the tool read — exact-token overlap.
	ctx := ScoreContext{Query: "please inspect internal/agent/relevance.go implementation", CurTurn: 1}
	hit := h.Score(ToolResult{ToolName: "read_file", Args: "internal/agent/relevance.go", Turn: 1}, ctx)
	miss := h.Score(ToolResult{ToolName: "read_file", Args: "unrelated/other/file.txt", Turn: 1}, ctx)
	if hit <= miss {
		t.Errorf("overlap should raise score: hit=%v miss=%v", hit, miss)
	}
}

func TestHeuristicSignalKeywordBoost(t *testing.T) {
	h := defaultHeuristicScorer()
	ctx := ScoreContext{CurTurn: 1}
	withErr := h.Score(ToolResult{ToolName: "bash", Result: "panic: nil pointer", Turn: 1}, ctx)
	clean := h.Score(ToolResult{ToolName: "bash", Result: "ok", Turn: 1}, ctx)
	if withErr <= clean {
		t.Errorf("signal keyword should boost: withErr=%v clean=%v", withErr, clean)
	}
}

func TestHeuristicDependencyReuseBoost(t *testing.T) {
	h := defaultHeuristicScorer()
	// A distinctive token from the result body reappears in a recent tool call's args.
	reused := ToolResult{ToolName: "grep", Result: "match in internal/agent/scheduler.go", Turn: 1}
	ctxReuse := ScoreContext{RecentArgs: []string{"internal/agent/scheduler.go"}, CurTurn: 1}
	ctxNoReuse := ScoreContext{RecentArgs: []string{"totally/other/path.txt"}, CurTurn: 1}
	if h.Score(reused, ctxReuse) <= h.Score(reused, ctxNoReuse) {
		t.Errorf("dependency reuse should boost score")
	}
}

func TestHeuristicRecencyDecayMonotonic(t *testing.T) {
	h := defaultHeuristicScorer()
	ctx := ScoreContext{CurTurn: 10}
	newest := h.Score(ToolResult{ToolName: "read_file", Turn: 10}, ctx)
	mid := h.Score(ToolResult{ToolName: "read_file", Turn: 5}, ctx)
	oldest := h.Score(ToolResult{ToolName: "read_file", Turn: 0}, ctx)
	if !(newest > mid && mid > oldest) {
		t.Errorf("recency decay should be monotonic: newest=%v mid=%v oldest=%v", newest, mid, oldest)
	}
}

func TestHeuristicDeterministicAndInRange(t *testing.T) {
	h := defaultHeuristicScorer()
	r := ToolResult{ToolName: "read_file", Args: "a/b/c.go foo_bar baz123", Result: "error: TODO here internal/x.go", Turn: 3}
	ctx := ScoreContext{Query: "look at c.go and foo_bar", RecentArgs: []string{"internal/x.go", "other"}, CurTurn: 5}
	first := h.Score(r, ctx)
	for i := 0; i < 20; i++ {
		if got := h.Score(r, ctx); got != first {
			t.Fatalf("non-deterministic score: %v != %v", got, first)
		}
	}
	if first < 0 || first > 1 {
		t.Errorf("score out of [0,1]: %v", first)
	}
}

func TestRecencyOnlyScorerIgnoresContent(t *testing.T) {
	h := recencyOnlyScorer()
	ctx := ScoreContext{Query: "anything", CurTurn: 4}
	// Two results at the same turn but wildly different content/tool-type must
	// score identically under the pure-recency scorer.
	a := h.Score(ToolResult{ToolName: "read_file", Result: "error TODO", Args: "x/y.go", Turn: 2}, ctx)
	b := h.Score(ToolResult{ToolName: "list_directory", Result: "", Args: "", Turn: 2}, ctx)
	if a != b {
		t.Errorf("recency-only must ignore content/tool-type: %v != %v", a, b)
	}
}

// --- ScoreContext derivation (Task 3) ---

func TestDeriveScoreContext(t *testing.T) {
	msgs := []model.Message{
		{Role: "user", Content: "first task"},
		{Role: "assistant", ToolCalls: []model.ToolCall{{ID: "1", Name: "read_file", Arguments: `{"path":"a.go"}`}}},
		{Role: "tool", ToolCallID: "1", Name: "read_file", Content: "contents of a"},
		{Role: "user", Content: "second task about b.go"},
		{Role: "assistant", ToolCalls: []model.ToolCall{{ID: "2", Name: "grep", Arguments: `{"q":"b.go"}`}}},
		{Role: "tool", ToolCallID: "2", Name: "grep", Content: "match"},
	}
	ctx := deriveScoreContext(msgs)
	if ctx.Query != "second task about b.go" {
		t.Errorf("Query = %q, want latest user message", ctx.Query)
	}
	if ctx.CurTurn != 1 {
		t.Errorf("CurTurn = %d, want 1", ctx.CurTurn)
	}
	// recentArgsTurns=2 covers turns 0 and 1, so both calls' args appear, newest-first.
	if len(ctx.RecentArgs) != 2 || ctx.RecentArgs[0] != `{"q":"b.go"}` {
		t.Errorf("RecentArgs = %v, want newest-first with both args", ctx.RecentArgs)
	}
}

func TestToolResultPairingAndTurns(t *testing.T) {
	msgs := []model.Message{
		{Role: "user", Content: "task"},
		{Role: "assistant", ToolCalls: []model.ToolCall{{ID: "1", Name: "read_file", Arguments: "ARGS1"}}},
		{Role: "tool", ToolCallID: "1", Name: "read_file", Content: "R1"},
		{Role: "tool", ToolCallID: "unpaired", Name: "bash", Content: "R2"}, // no matching call
	}
	turnOf := turnIndices(msgs)
	argsByID := argsByToolCallID(msgs)
	r1 := toolResultAt(msgs, 2, argsByID, turnOf)
	if r1.Args != "ARGS1" || r1.ToolName != "read_file" || r1.Result != "R1" {
		t.Errorf("paired result wrong: %+v", r1)
	}
	r2 := toolResultAt(msgs, 3, argsByID, turnOf)
	if r2.Args != "" {
		t.Errorf("unpaired tool result must have empty Args, got %q", r2.Args)
	}
	if turnOf[2] != 0 || turnOf[3] != 0 {
		t.Errorf("turn indices wrong: %v", turnOf)
	}
}
