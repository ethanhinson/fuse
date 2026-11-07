package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/ratelimit"
)

// The change 0036 observability surface renders the scheduler's pool and
// rate-gate counters into short, machine-built strings. These tests pin the
// format (the spec shape, omitting unset axes) and prove the strings reach the
// status bar and the agents overlay.

func TestPoolSummaryLineSpecShapeAndOmitsUnsetAxes(t *testing.T) {
	// Full shape: `session 3/5 slots · 2 queued · 310k/500k tokens`.
	got := poolSummaryLine(agent.PoolSnapshot{
		SlotsInUse: 3, SlotTotal: 5, Queued: 2,
		TokenSpend: 310_000, TokenQuota: 500_000,
	})
	if want := "session 3/5 slots · 2 queued · 310k/500k tokens"; got != want {
		t.Errorf("full pool line = %q, want %q", got, want)
	}

	// Unset axes are omitted: no slot cap, no queue, no token quota ⇒ nothing.
	if got := poolSummaryLine(agent.PoolSnapshot{SlotsInUse: 1}); got != "" {
		t.Errorf("all-unset pool line = %q, want empty", got)
	}

	// Only slots engaged (no queue, no token quota) ⇒ just the slots segment.
	if got := poolSummaryLine(agent.PoolSnapshot{SlotsInUse: 2, SlotTotal: 4}); got != "session 2/4 slots" {
		t.Errorf("slots-only session line = %q, want %q", got, "session 2/4 slots")
	}
}

func TestRateGateLineOmitsUnlimitedAxis(t *testing.T) {
	got := rateGateLine(ratelimit.ProviderUtilization{
		Provider: "deepseek", RPMCap: 6, RPMRemaining: 5, TPMCap: 600, TPMRemaining: 500,
	})
	if want := "deepseek 5/6 rpm · 500/600 tpm"; got != want {
		t.Errorf("rate line = %q, want %q", got, want)
	}
	// tpm unlimited (cap 0) ⇒ only the rpm segment; empty provider ⇒ "global".
	if got := rateGateLine(ratelimit.ProviderUtilization{RPMCap: 60, RPMRemaining: 59}); got != "global 59/60 rpm" {
		t.Errorf("rpm-only global line = %q, want %q", got, "global 59/60 rpm")
	}
}

func TestSchedulerSummaryLinesOrderingAndRateGate(t *testing.T) {
	snap := agent.SchedulerSnapshot{
		Pools: []agent.PoolSnapshot{{SlotsInUse: 1, SlotTotal: 4}},
	}
	util := []ratelimit.ProviderUtilization{{Provider: "", RPMCap: 60, RPMRemaining: 60}}
	lines := schedulerSummaryLines(snap, util)
	if len(lines) != 2 {
		t.Fatalf("summary lines = %#v, want 2", lines)
	}
	if !strings.HasPrefix(lines[0], "session ") {
		t.Errorf("first line = %q, want the session pool first", lines[0])
	}
	if !strings.HasPrefix(lines[1], "global ") {
		t.Errorf("second line = %q, want the rate-gate line last", lines[1])
	}
}

// TestSchedulerStatusSegmentPicksBusiest asserts the status-bar segment surfaces the
// busiest engaged pool (max SlotsInUse, tie → more Queued).
func TestSchedulerStatusSegmentPicksBusiest(t *testing.T) {
	snap := agent.SchedulerSnapshot{
		Pools: []agent.PoolSnapshot{
			{SlotsInUse: 1, SlotTotal: 5},
			{SlotsInUse: 4, SlotTotal: 5},
		},
	}
	if got := schedulerStatusSegment(snap); got != "session 4/5 slots" {
		t.Errorf("segment = %q, want the busiest pool", got)
	}

	// SlotsInUse tie ⇒ more Queued wins.
	tie := agent.SchedulerSnapshot{
		Pools: []agent.PoolSnapshot{
			{SlotsInUse: 3, SlotTotal: 5, Queued: 1},
			{SlotsInUse: 3, SlotTotal: 5, Queued: 5},
		},
	}
	if got := schedulerStatusSegment(tie); got != "session 3/5 slots · 5 queued" {
		t.Errorf("segment = %q, want the more-queued pool on a slots tie", got)
	}

	// A pool with no renderable axis is not "engaged" and must be skipped.
	skip := agent.SchedulerSnapshot{
		Pools: []agent.PoolSnapshot{
			{SlotsInUse: 9}, // SlotTotal 0 ⇒ no renderable axis
			{SlotsInUse: 2, SlotTotal: 4},
		},
	}
	if got := schedulerStatusSegment(skip); got != "session 2/4 slots" {
		t.Errorf("segment = %q, want the engaged session pool (unrenderable pool skipped)", got)
	}

	// No engaged pool ⇒ empty segment.
	if got := schedulerStatusSegment(agent.SchedulerSnapshot{}); got != "" {
		t.Errorf("segment = %q, want empty when no pool is engaged", got)
	}
}

// TestStatusBarShowsSchedulerSegment: with a session token ceiling set and a
// child running, the status bar surfaces the scheduler segment (session slots +
// token spend) alongside the running counter.
func TestStatusBarShowsSchedulerSegment(t *testing.T) {
	tree := agent.NewAgentTreeWithConcurrency("root", "alpha", 4)
	tree.Scheduler().SetSessionTokens(500)
	root := tree.Node(tree.RootID())
	block := make(chan struct{})
	s := agent.NewSpawner(agent.WithTree(tree), agent.WithNode(root),
		agent.WithChildBuilder(func(ctx context.Context, _ agent.SpawnOpts, child *agent.AgentNode, _ *agent.AgentTree) (string, error) {
			child.UpdateTokens(200, 100) // session spend 300
			select {
			case <-block:
			case <-ctx.Done():
			}
			return "", nil
		}))
	h, err := s.Spawn(context.Background(), agent.SpawnOpts{Label: "w"})
	if err != nil {
		t.Fatal(err)
	}
	// Wait until the child is running and has recorded its token spend.
	deadline := time.After(2 * time.Second)
	for {
		if r, _ := tree.ActiveCounts(); r >= 1 && tree.SessionTokens() >= 300 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("child never started / recorded tokens")
		case <-time.After(2 * time.Millisecond):
		}
	}

	m := sized(NewShellModel("alpha", false, "dark", testRegistry(), nil, nilBuilder, permissions.NewSessionMode(permissions.ModeSmart), true))
	m = m.WithTree(tree)
	view := ansiRE.ReplaceAllString(m.View(), "")
	if !strings.Contains(view, "session 1/4 slots") {
		t.Errorf("status bar missing session slots segment: %q", view)
	}
	if !strings.Contains(view, "300/500 tokens") {
		t.Errorf("status bar missing session token segment: %q", view)
	}

	close(block)
	h.Wait()
}

// TestAgentsOverlayShowsSchedulerHeader: the agents overlay tree pane renders the
// per-pool scheduler summary as a header block above the tree.
func TestAgentsOverlayShowsSchedulerHeader(t *testing.T) {
	tree := agent.NewAgentTreeWithConcurrency("root", "alpha", 4)
	tree.Scheduler().SetSessionTokens(500)
	root := tree.Node(tree.RootID())
	block := make(chan struct{})
	s := agent.NewSpawner(agent.WithTree(tree), agent.WithNode(root),
		agent.WithChildBuilder(func(ctx context.Context, _ agent.SpawnOpts, child *agent.AgentNode, _ *agent.AgentTree) (string, error) {
			child.UpdateTokens(200, 100)
			select {
			case <-block:
			case <-ctx.Done():
			}
			return "", nil
		}))
	h, err := s.Spawn(context.Background(), agent.SpawnOpts{Label: "w"})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.After(2 * time.Second)
	for {
		if r, _ := tree.ActiveCounts(); r >= 1 && tree.SessionTokens() >= 300 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("child never started / recorded tokens")
		case <-time.After(2 * time.Millisecond):
		}
	}

	am := NewAgentsModel(tree, nil)
	am.width = 80
	am.height = 24
	view := ansiRE.ReplaceAllString(am.View(), "")
	if !strings.Contains(view, "session 1/4 slots") {
		t.Errorf("agents overlay missing scheduler header: %q", view)
	}
	if !strings.Contains(view, "300/500 tokens") {
		t.Errorf("agents overlay missing token spend in header: %q", view)
	}

	close(block)
	h.Wait()
}
