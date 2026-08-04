package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ethanhinson/fuse/internal/tools"
)

func TestTeaRendererAssistant(t *testing.T) {
	ch := make(chan tea.Msg, 1)
	r := NewTeaRenderer(ch)
	r.Assistant("hello")
	msg := <-ch
	am, ok := msg.(AssistantMsg)
	if !ok || am.Text != "hello" {
		t.Fatalf("want AssistantMsg{hello}, got %#v", msg)
	}
}

func TestTeaRendererToolCall(t *testing.T) {
	ch := make(chan tea.Msg, 1)
	r := NewTeaRenderer(ch)
	r.ToolCall("bash", "ls -la")
	msg := <-ch
	tc, ok := msg.(ToolCallMsg)
	if !ok || tc.Name != "bash" || tc.Args != "ls -la" {
		t.Fatalf("want ToolCallMsg{bash, ls -la}, got %#v", msg)
	}
}

func TestTeaRendererToolResult(t *testing.T) {
	ch := make(chan tea.Msg, 2)
	r := NewTeaRenderer(ch)
	r.ToolResult("bash", tools.Result{IsError: false, Output: "ok"})
	r.ToolResult("bash", tools.Result{IsError: true, Output: "boom"})
	m1 := (<-ch).(ToolResultMsg)
	if m1.Name != "bash" || m1.IsError || m1.Output != "ok" {
		t.Fatalf("first result mismatch: %#v", m1)
	}
	m2 := (<-ch).(ToolResultMsg)
	if !m2.IsError || m2.Output != "boom" {
		t.Fatalf("second result mismatch: %#v", m2)
	}
}

func TestTeaRendererErrorf(t *testing.T) {
	ch := make(chan tea.Msg, 1)
	r := NewTeaRenderer(ch)
	r.Errorf("failed %d times: %s", 3, "nope")
	msg := <-ch
	em, ok := msg.(AgentErrMsg)
	if !ok || em.Err != "failed 3 times: nope" {
		t.Fatalf("want formatted AgentErrMsg, got %#v", msg)
	}
}
