package main

import (
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
