package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/model"
)

// driveHistory seeds a history that is ALREADY over budget for an 8000-token
// window (budget 6800, protect 2000), so the first turn of the real loop prunes.
// The OLDEST tool result is a read_file on internal/agent/relevance.go — the path
// the final user turn names — followed by trivial newer filler. Each result is
// ~1000 tokens (under the 2000-token protect budget) so score order, not just
// recency, decides which ~2 survive. Because the important result is the oldest,
// pure recency sacrifices it first; a relevance scorer must rescue it.
//
// (The loop's token accounting resets each turn to lastUsage + messages-since,
// and scripted responses report zero usage — so the estimate never accumulates
// across turns. The seed must therefore be over budget on turn 0, matching how
// every other prune test drives the loop.)
func driveHistory() []model.Message {
	filler := strings.Repeat("x", 4000) // ~1000 tokens each
	msgs := []model.Message{
		{Role: "user", Content: "please work on internal/agent/relevance.go"},
		{Role: "assistant", ToolCalls: []model.ToolCall{{ID: "1", Name: "read_file", Arguments: "internal/agent/relevance.go"}}},
		// OLDEST + IMPORTANT: content names the query path.
		{Role: "tool", ToolCallID: "1", Name: "read_file", Content: "package agent // internal/agent/relevance.go " + filler},
	}
	// Seven trivial, newer list_directory results (~7000 tok) so the total is well
	// over the 6800 budget and the important one is clearly the oldest.
	for i, id := range []string{"s0", "s1", "s2", "s3", "s4", "s5", "s6"} {
		msgs = append(msgs,
			model.Message{Role: "assistant", ToolCalls: []model.ToolCall{{ID: id, Name: "list_directory", Arguments: "/seed" + string(rune('0'+i))}}},
			model.Message{Role: "tool", ToolCallID: id, Name: "list_directory", Content: filler},
		)
	}
	return msgs
}

// importantSurvived reports whether the old read_file on the query path is still
// present (not stubbed) in the returned history.
func importantSurvived(hist []model.Message) bool {
	for _, m := range hist {
		if m.Role == "tool" && m.ToolCallID == "1" {
			return m.Content != prunedStub
		}
	}
	return false
}

// TestDriveRelevancePruneRescuesImportant drives the REAL agent loop (a.Run)
// until pruneOldToolResults fires, and proves the relevance scorer changes the
// outcome versus a pure-recency control:
//
//   - heuristic scorer (default, 0% floor): the old-but-important read_file on
//     the query path SURVIVES a real over-budget run.
//   - recency-only scorer (same run): the same old result is SACRIFICED.
//
// The contrast is the end-to-end proof that relevance — not just recency —
// drove the rescue through the production loop, not a direct prune call.
func TestDriveRelevancePruneRescuesImportant(t *testing.T) {
	// The seed history is already over budget, so the loop prunes on turn 0; a
	// single natural stop ends the run right after that prune.
	script := func() *scriptedCompleter {
		return &scriptedCompleter{responses: []model.CompletionResp{{Content: "done"}}}
	}

	// --- Arm A: default heuristic scorer, 0% floor (relevance decides) ---
	relevA := New(script(), &fakeExec{}, nopRenderer{}, "m", "", 12, 100)
	relevA.ContextWindow = 8000 // budget 6800, protect 2000 — ~2 results survive
	relevA.SetRecencyFloorPct(0)
	histA, errA := relevA.Run(context.Background(), driveHistory())
	if errA != nil {
		t.Fatalf("relevance-arm run error: %v", errA)
	}
	if !importantSurvived(histA) {
		t.Error("relevance arm: important old read_file on the query path should have been rescued through the driven loop, but it was stubbed")
	}

	// --- Arm B: pure-recency scorer, same run (control) ---
	recB := New(script(), &fakeExec{}, nopRenderer{}, "m", "", 12, 100)
	recB.ContextWindow = 8000
	recB.SetRelevanceScorer(recencyOnlyScorer())
	recB.SetRecencyFloorPct(0)
	histB, errB := recB.Run(context.Background(), driveHistory())
	if errB != nil {
		t.Fatalf("recency-arm run error: %v", errB)
	}
	if importantSurvived(histB) {
		t.Error("recency control: the old result should have been sacrificed by pure recency — if it survived, the run never crossed budget and the test proves nothing")
	}

	// Sanity: a prune actually happened (at least one tool result got stubbed).
	stubbed := 0
	for _, m := range histA {
		if m.Content == prunedStub {
			stubbed++
		}
	}
	if stubbed == 0 {
		t.Fatal("no tool result was stubbed in the relevance arm — the driven loop never pruned, so the assertion is vacuous")
	}
}
