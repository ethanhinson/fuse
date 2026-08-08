package agent

import (
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/model"
)

// legacyRecencyPrune is the pre-0028 recency-only prune, kept here as the oracle
// for the no-op degeneration invariant: relevance-aware pruning with a
// pure-recency scorer must produce a byte-identical stub set.
func legacyRecencyPrune(messages []model.Message, protectTokens int) int {
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

func stubMask(messages []model.Message) []bool {
	out := make([]bool, len(messages))
	for i, m := range messages {
		out[i] = m.Content == prunedStub
	}
	return out
}

// TestPruneNoOpDegeneration: with a pure-recency scorer at any floor, the exact
// set of stubbed messages equals the legacy recency-only prune. This is the
// safety invariant the whole design rests on.
func TestPruneNoOpDegeneration(t *testing.T) {
	build := func() []model.Message {
		big := strings.Repeat("x", 4000) // ~1000 tokens each
		return []model.Message{
			{Role: "user", Content: "task"},
			{Role: "assistant", ToolCalls: []model.ToolCall{{ID: "1", Name: "read_file", Arguments: "a"}}},
			{Role: "tool", ToolCallID: "1", Name: "read_file", Content: big},
			{Role: "assistant", ToolCalls: []model.ToolCall{{ID: "2", Name: "grep", Arguments: "b"}}},
			{Role: "tool", ToolCallID: "2", Name: "grep", Content: big},
			{Role: "assistant", ToolCalls: []model.ToolCall{{ID: "3", Name: "list_directory", Arguments: "c"}}},
			{Role: "tool", ToolCallID: "3", Name: "list_directory", Content: big},
		}
	}
	for _, protect := range []int{0, 1000, 1500, 2000, 3000, 5000} {
		for _, floor := range []int{0, 50, 100} {
			legacy := build()
			freedLegacy := legacyRecencyPrune(legacy, protect)

			got := build()
			freedGot := pruneOldToolResults(got, protect, recencyOnlyScorer(), floor)

			if freedGot != freedLegacy {
				t.Errorf("protect=%d floor=%d: freed=%d, want legacy %d", protect, floor, freedGot, freedLegacy)
			}
			lm, gm := stubMask(legacy), stubMask(got)
			for i := range lm {
				if lm[i] != gm[i] {
					t.Errorf("protect=%d floor=%d: stub mask differs at %d: got %v want %v (masks got=%v legacy=%v)",
						protect, floor, i, gm[i], lm[i], gm, lm)
				}
			}
		}
	}
}

// TestPruneRescuesImportantOldResult: an old read_file whose path is named in
// the current query survives, while a trivial recent list_directory is stubbed,
// when the budget forces a choice between them.
func TestPruneRescuesImportantOldResult(t *testing.T) {
	big := strings.Repeat("y", 4000) // ~1000 tokens each
	msgs := []model.Message{
		{Role: "user", Content: "task"},
		{Role: "assistant", ToolCalls: []model.ToolCall{{ID: "1", Name: "read_file", Arguments: "internal/agent/relevance.go"}}},
		{Role: "tool", ToolCallID: "1", Name: "read_file", Content: "package agent " + big}, // OLD, important
		{Role: "assistant", ToolCalls: []model.ToolCall{{ID: "2", Name: "list_directory", Arguments: "tmp"}}},
		{Role: "tool", ToolCallID: "2", Name: "list_directory", Content: big}, // RECENT, trivial
		{Role: "user", Content: "now fix internal/agent/relevance.go"},
	}
	// Budget ~1000 tokens with a 0% floor forces a single winner by relevance.
	// The old read_file overlaps the query path token; the recent list_directory
	// does not — relevance should rescue the old one.
	freed := pruneOldToolResults(msgs, 1000, defaultHeuristicScorer(), 0)
	if freed == 0 {
		t.Fatal("expected some pruning")
	}
	if msgs[2].Content == prunedStub {
		t.Error("important old read_file should be rescued, but it was stubbed")
	}
	if msgs[4].Content != prunedStub {
		t.Error("trivial recent list_directory should be stubbed")
	}
}

// TestPruneFloorNeverViolated: the newest results up to the floor are always
// protected regardless of relevance score. A high-scoring old result must not
// displace a low-scoring newest result inside the floor.
func TestPruneFloorNeverViolated(t *testing.T) {
	big := strings.Repeat("z", 4000) // ~1000 tokens each
	msgs := []model.Message{
		{Role: "user", Content: "task about internal/agent/relevance.go"},
		{Role: "assistant", ToolCalls: []model.ToolCall{{ID: "1", Name: "read_file", Arguments: "internal/agent/relevance.go"}}},
		{Role: "tool", ToolCallID: "1", Name: "read_file", Content: "package agent " + big}, // OLD high-relevance
		{Role: "assistant", ToolCalls: []model.ToolCall{{ID: "2", Name: "list_directory", Arguments: "tmp"}}},
		{Role: "tool", ToolCallID: "2", Name: "list_directory", Content: big}, // NEWEST trivial
	}
	// Floor=100% of a 1000-token budget guarantees exactly the newest ~1000
	// tokens (the trivial list_directory) is protected, and nothing remains for
	// the relevance fill — the old high-relevance result is stubbed.
	pruneOldToolResults(msgs, 1000, defaultHeuristicScorer(), 100)
	if msgs[4].Content == prunedStub {
		t.Error("floor must protect the newest result regardless of score")
	}
	if msgs[2].Content != prunedStub {
		t.Error("with a 100% recency floor, the old result gets no relevance budget and is stubbed")
	}
}

// TestNewAgentHasDefaultScorer: New() installs a non-nil heuristic scorer so the
// prune step never runs without a scorer.
func TestNewAgentHasDefaultScorer(t *testing.T) {
	a := New(&scriptedCompleter{}, &fakeExec{}, nopRenderer{}, "m", "", 0, 0)
	if a.relevanceScorer == nil {
		t.Fatal("New() must install a default relevance scorer")
	}
	if a.recencyFloorPct() != defaultRecencyFloorPct {
		t.Errorf("default recency floor = %d, want %d", a.recencyFloorPct(), defaultRecencyFloorPct)
	}
	a.SetRelevanceScorer(nil)
	if a.relevanceScorer == nil {
		t.Error("SetRelevanceScorer(nil) must keep a non-nil default")
	}
	a.SetRecencyFloorPct(200)
	if a.recencyFloorPct() != 100 {
		t.Errorf("out-of-range floor should clamp to 100, got %d", a.recencyFloorPct())
	}
}
