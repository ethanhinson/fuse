package tui

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/agent"
)

// base is a fixed wall-clock anchor so every offset in these tests is exact.
var turnsBase = time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)

func evtAt(kind agent.EventKind, name string, ts time.Time) agent.AgentEvent {
	return agent.AgentEvent{Kind: kind, Name: name, TS: ts}
}

// twoTurnRoot reproduces the reported defect's shape: a root whose StartedAt is
// turn 1's start and whose turn-1 events all precede turn 2's StartedAt.
func twoTurnRoot() (agent.NodeView, []agent.AgentEvent) {
	t1 := turnsBase
	t2 := turnsBase.Add(6*time.Hour + 40*time.Minute + 13*time.Second) // ~24013s later
	n := agent.NodeView{
		ID:        "root",
		StartedAt: t1,
		Turns: []agent.TurnMark{
			{Turn: 1, StartedAt: t1, EndedAt: t1.Add(30 * time.Second)},
			{Turn: 2, StartedAt: t2},
		},
	}
	events := []agent.AgentEvent{
		evtAt(agent.KindAssistant, "prompt-1", t1),
		evtAt(agent.KindToolCall, "tool-1", t1.Add(2500*time.Millisecond)),
		evtAt(agent.KindToolResult, "tool-1", t1.Add(12*time.Second)),
		evtAt(agent.KindAssistant, "prompt-2", t2),
		evtAt(agent.KindToolCall, "tool-2", t2.Add(3*time.Second)),
	}
	return n, events
}

func TestEventOffsetNeverNegativeAcrossTurns(t *testing.T) {
	n, events := twoTurnRoot()

	// The detail pane is flat (turn grouping moved to the left tree), but each
	// event's offset is still turn-relative — never negative, and equal to
	// TS - its own turn's start.
	wantStarts := []time.Time{
		n.Turns[0].StartedAt, n.Turns[0].StartedAt, n.Turns[0].StartedAt,
		n.Turns[1].StartedAt, n.Turns[1].StartedAt,
	}
	for i, evt := range events {
		got := eventOffset(n, evt.TS)
		if strings.HasPrefix(got, "-") {
			t.Errorf("event %d rendered a NEGATIVE offset %q (the reported bug)", i, got)
		}
		want := fmt.Sprintf("%05.1fs", evt.TS.Sub(wantStarts[i]).Seconds())
		if got != want {
			t.Errorf("event %d offset = %q, want %q", i, got, want)
		}
	}
}

// TestEventOffsetNegativeWhenStartedAtTracksLatestTurn is the literal shape of
// the reported defect: a root whose StartedAt was clobbered to the CURRENT turn's
// start while turn-1 events still sit far behind it. Offsetting against
// n.StartedAt yields [-24013.7s]; turn attribution must not.
func TestEventOffsetNegativeWhenStartedAtTracksLatestTurn(t *testing.T) {
	n, events := twoTurnRoot()
	n.StartedAt = n.Turns[1].StartedAt // the pre-fix clobbered value

	for i, evt := range events {
		if got := eventOffset(n, evt.TS); strings.HasPrefix(got, "-") {
			t.Errorf("event %d rendered a negative offset %q (the reported bug)", i, got)
		}
	}
	if got, want := eventOffset(n, events[0].TS), "000.0s"; got != want {
		t.Errorf("turn-1 first event offset = %q, want %q", got, want)
	}
	if got, want := eventOffset(n, events[2].TS), "012.0s"; got != want {
		t.Errorf("turn-1 late event offset = %q, want %q", got, want)
	}
}

func TestTurnStartForAttributesByTimestamp(t *testing.T) {
	n, events := twoTurnRoot()
	for i, evt := range events {
		want := n.Turns[0].StartedAt
		if i >= 3 {
			want = n.Turns[1].StartedAt
		}
		if got := turnStartFor(n, evt.TS); !got.Equal(want) {
			t.Errorf("event %d attributed to %v, want %v", i, got, want)
		}
	}
}

func TestTurnStartForBoundaryBelongsToLaterTurn(t *testing.T) {
	n, _ := twoTurnRoot()
	if got := turnStartFor(n, n.Turns[1].StartedAt); !got.Equal(n.Turns[1].StartedAt) {
		t.Fatalf("boundary event attributed to %v, want turn 2 start %v", got, n.Turns[1].StartedAt)
	}
}

func TestEventOffsetLegacyEmptyTurnsUnchanged(t *testing.T) {
	n := agent.NodeView{ID: "child", StartedAt: turnsBase}
	for _, d := range []time.Duration{0, 1500 * time.Millisecond, 90 * time.Second} {
		ts := turnsBase.Add(d)
		want := fmt.Sprintf("%05.1fs", ts.Sub(n.StartedAt).Seconds())
		if got := eventOffset(n, ts); got != want {
			t.Errorf("legacy offset for +%v = %q, want %q", d, got, want)
		}
		if got := turnStartFor(n, ts); !got.Equal(n.StartedAt) {
			t.Errorf("legacy turnStartFor = %v, want StartedAt %v", got, n.StartedAt)
		}
	}
}

func TestEventOffsetPreTurnOneClampsToZero(t *testing.T) {
	n, _ := twoTurnRoot()
	early := n.Turns[0].StartedAt.Add(-42 * time.Second)
	if got := turnStartFor(n, early); !got.Equal(n.Turns[0].StartedAt) {
		t.Fatalf("pre-turn-1 event attributed to %v, want turn 1 start", got)
	}
	got := eventOffset(n, early)
	if strings.HasPrefix(strings.TrimSpace(got), "-") {
		t.Fatalf("pre-turn-1 event rendered negative offset %q", got)
	}
	if v := parseOffsetSeconds(t, got); math.Abs(v) > 0.05 {
		t.Fatalf("pre-turn-1 offset = %q (%v s), want 0.0s", got, v)
	}
}

func parseOffsetSeconds(t *testing.T, s string) float64 {
	t.Helper()
	var v float64
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%fs", &v); err != nil {
		t.Fatalf("unparseable offset %q: %v", s, err)
	}
	return v
}

func TestNodeElapsedUsesLastTurnNotWholeSession(t *testing.T) {
	n, _ := twoTurnRoot()
	n.EndedAt = n.Turns[1].StartedAt.Add(90 * time.Second)
	n.Turns[1].EndedAt = n.EndedAt

	got := nodeElapsed(n)
	want := "1m30s"
	if got != want {
		t.Errorf("nodeElapsed = %q, want %q (turn 2's duration, not the whole session)", got, want)
	}
}

func TestNodeElapsedInFlightLastTurnCountsFromCurrentMark(t *testing.T) {
	n, _ := twoTurnRoot()
	n.Turns[1].StartedAt = time.Now().Add(-5 * time.Second)
	// EndedAt left zero: turn still in flight.

	got := nodeElapsed(n)
	if got == "" || got == "–" {
		t.Fatalf("nodeElapsed = %q, want a live elapsed reading from the current mark", got)
	}
	v := parseOffsetSecondsSuffix(t, got, "s")
	if v < 4 || v > 10 {
		t.Fatalf("nodeElapsed = %q (%v s), want ~5s from the in-flight turn's StartedAt", got, v)
	}
}

func TestNodeElapsedEmptyTurnsUnchanged(t *testing.T) {
	n := agent.NodeView{
		ID:        "child",
		StartedAt: turnsBase,
		EndedAt:   turnsBase.Add(12 * time.Second),
	}
	if got, want := nodeElapsed(n), "12s"; got != want {
		t.Errorf("nodeElapsed = %q, want %q", got, want)
	}
}

func TestNodeElapsedZeroStartedAtEmptyTurnsStillDash(t *testing.T) {
	n := agent.NodeView{ID: "child"}
	if got, want := nodeElapsed(n), "–"; got != want {
		t.Errorf("nodeElapsed = %q, want %q", got, want)
	}
}

func TestNodeElapsedTurnAwareMinuteCrossingFormat(t *testing.T) {
	n, _ := twoTurnRoot()
	n.Turns[1].EndedAt = n.Turns[1].StartedAt.Add(75 * time.Second)

	if got, want := nodeElapsed(n), "1m15s"; got != want {
		t.Errorf("nodeElapsed = %q, want %q", got, want)
	}
}

func parseOffsetSecondsSuffix(t *testing.T, s, suffix string) float64 {
	t.Helper()
	var v float64
	if _, err := fmt.Sscanf(strings.TrimSuffix(s, suffix), "%f", &v); err != nil {
		t.Fatalf("unparseable elapsed %q: %v", s, err)
	}
	return v
}

// stripANSITurns removes SGR escape sequences so offsets can be read from a
// styled row.
func stripANSITurns(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
