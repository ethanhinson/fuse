package agent

import (
	"testing"

	"github.com/ethanhinson/fuse/internal/model"
)

// TestUserTurns_KeepsOnlyUserRoles proves userTurns is defense-in-depth for the
// auto-mode classifier ctx-carry seam: it keeps user turns in order and drops
// system, assistant, and tool-result messages.
func TestUserTurns_KeepsOnlyUserRoles(t *testing.T) {
	in := []model.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "u1"},
		{Role: "assistant", Content: "a1"},
		{Role: "tool", ToolCallID: "c1", Name: "bash", Content: "tool result"},
		{Role: "user", Content: "u2"},
	}
	got := userTurns(in)
	if len(got) != 2 {
		t.Fatalf("expected 2 user turns, got %d: %#v", len(got), got)
	}
	if got[0].Content != "u1" || got[1].Content != "u2" {
		t.Fatalf("user turns out of order or altered: %#v", got)
	}
	for _, m := range got {
		if m.Role != "user" {
			t.Errorf("non-user role leaked through userTurns: %q", m.Role)
		}
	}
}

// TestUserTurns_NilAndEmpty proves userTurns returns nil for empty/no-user input.
func TestUserTurns_NilAndEmpty(t *testing.T) {
	if got := userTurns(nil); got != nil {
		t.Errorf("nil input should yield nil, got %#v", got)
	}
	onlyNonUser := []model.Message{
		{Role: "assistant", Content: "a"},
		{Role: "tool", Content: "t"},
	}
	if got := userTurns(onlyNonUser); got != nil {
		t.Errorf("no user turns should yield nil, got %#v", got)
	}
}
