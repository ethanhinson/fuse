package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/tools"
)

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
