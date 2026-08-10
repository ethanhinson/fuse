package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/tools"
)

func TestResolveMaxTurns(t *testing.T) {
	ptr := func(n int) *int { return &n }
	tests := []struct {
		name        string
		cfg         *int
		interactive bool
		want        int
	}{
		{"unset interactive is unlimited", nil, true, 0},
		{"unset headless backstops at 100", nil, false, 100},
		{"explicit zero is unlimited (interactive)", ptr(0), true, 0},
		{"explicit zero is unlimited (headless)", ptr(0), false, 0},
		{"explicit positive caps (interactive)", ptr(9), true, 9},
		{"explicit positive caps (headless)", ptr(9), false, 9},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveMaxTurns(tt.cfg, tt.interactive); got != tt.want {
				t.Errorf("resolveMaxTurns(%v, %v) = %d, want %d", tt.cfg, tt.interactive, got, tt.want)
			}
		})
	}
}

// TestOneShotBudgetPostureUnderApproveAll is the regression for the review
// CONCERN: `fuse "task" --approve-all` on a TTY must NOT resolve to the
// interactive (unlimited-turns, auto-continue-loop) posture. --approve-all is a
// scripted "don't ask me" posture, so the turn/loop budget must be headless: an
// unset max_turns backstops at 100, and the loop hook is nil so a doom-loop trip
// aborts with the structured error instead of being auto-approved forever.
// Explicit max_turns config still wins.
func TestOneShotBudgetPostureUnderApproveAll(t *testing.T) {
	ptr := func(n int) *int { return &n }
	tests := []struct {
		name        string
		tty         bool
		approveAll  bool
		wantPosture bool // the resolved turn/loop interactive posture
	}{
		{"TTY, no approve-all ⇒ interactive", true, false, true},
		{"TTY, --approve-all ⇒ headless (the CONCERN)", true, true, false},
		{"piped, no approve-all ⇒ headless", false, false, false},
		{"piped, --approve-all ⇒ headless", false, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := oneShotBudgetInteractive(tt.tty, tt.approveAll)
			if got != tt.wantPosture {
				t.Fatalf("oneShotBudgetInteractive(tty=%v, approveAll=%v) = %v, want %v",
					tt.tty, tt.approveAll, got, tt.wantPosture)
			}
			// The doom-loop hook must be nil in the headless posture: a trip
			// aborts rather than routing to auto-approve.
			hook := loopApprovalFor(autoApprove, got)
			if tt.wantPosture && hook == nil {
				t.Error("interactive posture must wire a loop force-through hook")
			}
			if !tt.wantPosture && hook != nil {
				t.Error("headless posture must leave the loop hook nil (abort on trip)")
			}
			// An unset max_turns must backstop at 100 in the headless posture,
			// stay unlimited (0) interactive; an explicit cap always wins.
			if got := resolveMaxTurns(nil, got); tt.wantPosture && got != 0 {
				t.Errorf("interactive unset max_turns = %d, want 0 (unlimited)", got)
			} else if !tt.wantPosture && got != headlessTurnBackstop {
				t.Errorf("headless unset max_turns = %d, want %d (backstop)", got, headlessTurnBackstop)
			}
			if got := resolveMaxTurns(ptr(9), got); got != 9 {
				t.Errorf("explicit max_turns must win regardless of posture, got %d", got)
			}
		})
	}
}

func TestChildResultReturnsPartialOnMaxTurns(t *testing.T) {
	msgs := []model.Message{
		{Role: "user", Content: "task"},
		{Role: "assistant", Content: "partial findings so far"},
	}
	out, err := childResult(msgs, agent.ErrMaxTurns)
	if err != nil {
		t.Fatalf("budget exhaustion should not be a bare error, got %v", err)
	}
	if !strings.Contains(out, "partial findings so far") {
		t.Errorf("partial transcript discarded: %q", out)
	}
	if !strings.Contains(out, "stopped:") {
		t.Errorf("missing stop-reason marker: %q", out)
	}
}

func TestChildResultPropagatesOtherErrors(t *testing.T) {
	boom := errors.New("gateway exploded")
	out, err := childResult([]model.Message{{Role: "assistant", Content: "x"}}, boom)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
	if out != "" {
		t.Errorf("out = %q, want empty on hard error", out)
	}
}

func TestChildToolRegistryRejectsUnknownNames(t *testing.T) {
	reg := tools.NewRegistry()
	for _, tl := range tools.DefaultTools() {
		reg.Register(tl)
	}
	if _, err := childToolRegistry(reg, []string{"read_file", "reed_file"}); err == nil {
		t.Fatal("unknown tool name should fail the spawn")
	} else if !strings.Contains(err.Error(), "reed_file") {
		t.Errorf("error should name the unknown tool: %v", err)
	}
	sub, err := childToolRegistry(reg, []string{"read_file"})
	if err != nil {
		t.Fatalf("valid subset failed: %v", err)
	}
	if sub == nil {
		t.Fatal("nil registry for valid subset")
	}
}

func TestShouldWireChildSpawn(t *testing.T) {
	cases := []struct {
		name      string
		requested []string
		want      bool
	}{
		{"empty inherits all", nil, true},
		{"empty slice inherits all", []string{}, true},
		{"subset without spawn_agent", []string{"read_file", "web_search"}, false},
		{"subset with spawn_agent", []string{"read_file", "spawn_agent"}, true},
		{"only spawn_agent", []string{"spawn_agent"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldWireChildSpawn(tc.requested); got != tc.want {
				t.Errorf("shouldWireChildSpawn(%v) = %v, want %v", tc.requested, got, tc.want)
			}
		})
	}
}

// TestChildToolRegistryOmitsSpawnWhenSubsetOmitsIt pins the 0034 folded-in fix at
// the registry seam: a subset that names only read_file yields a child registry
// with NO spawn_agent (before 0034, Subset force-included it).
func TestChildToolRegistryOmitsSpawnWhenSubsetOmitsIt(t *testing.T) {
	reg := tools.NewRegistry()
	for _, tl := range tools.DefaultTools() {
		reg.Register(tl)
	}
	reg.Register(tools.NewSpawnAgentTool(func(ctx context.Context, req tools.SpawnRequest) (tools.SpawnHandle, error) {
		return nil, nil
	}))

	sub, err := childToolRegistry(reg, []string{"read_file"})
	if err != nil {
		t.Fatalf("subset failed: %v", err)
	}
	for _, s := range sub.Schemas() {
		if s.Name == "spawn_agent" {
			t.Fatal("child subset omitting spawn_agent must not contain it")
		}
	}

	// Empty names inherits all, including spawn_agent.
	inh, err := childToolRegistry(reg, nil)
	if err != nil {
		t.Fatalf("clone failed: %v", err)
	}
	found := false
	for _, s := range inh.Schemas() {
		if s.Name == "spawn_agent" {
			found = true
		}
	}
	if !found {
		t.Error("empty-names child should inherit spawn_agent")
	}
}
