package tui

import (
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/agent"
)

// TestTurnIndexForBoundarySkew pins the change-0067 fix: a turn's first event can
// be stamped a few ms BEFORE that turn's mark (the shell stamps the mark at
// prompt-submit; the async run goroutine stamps the event) — but it is after the
// previous turn's EndedAt, so it belongs to the later turn, not the settled one.
// A StartedAt-only rule mis-buckets it into the previous (collapsed) turn and the
// running turn renders empty — the reported "turn 2 is empty" defect.
func TestTurnIndexForBoundarySkew(t *testing.T) {
	base := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	n := agent.NodeView{
		StartedAt: base,
		Turns: []agent.TurnMark{
			{Turn: 1, StartedAt: base, EndedAt: base.Add(4 * time.Second)},
			// Turn 2's mark is stamped 37ms AFTER its first event fires.
			{Turn: 2, StartedAt: base.Add(6 * time.Second).Add(37 * time.Millisecond)},
		},
	}

	// Turn 2's first event: after turn 1 ended (4s), before turn 2's mark (6.037s).
	skewed := base.Add(6 * time.Second)
	if got := turnIndexFor(n, skewed); got != 1 {
		t.Errorf("skewed turn-2 first event bucketed to turn index %d, want 1 (turn 2)", got)
	}

	// A genuinely turn-1 event (before turn 1 ended) still buckets to turn 1.
	if got := turnIndexFor(n, base.Add(2*time.Second)); got != 0 {
		t.Errorf("turn-1 event bucketed to index %d, want 0", got)
	}

	// A clearly-turn-2 event (after turn 2's mark) buckets to turn 2.
	if got := turnIndexFor(n, base.Add(7*time.Second)); got != 1 {
		t.Errorf("turn-2 event bucketed to index %d, want 1", got)
	}

	// An event that predates turn 1 anchors to turn 1 (non-negative offset).
	if got := turnIndexFor(n, base.Add(-1*time.Second)); got != 0 {
		t.Errorf("pre-turn-1 event bucketed to index %d, want 0 (anchor)", got)
	}
}
