package tui

import (
	tea "github.com/charmbracelet/bubbletea"

	"github.com/ethanhinson/fuse/internal/model"
)

// Message types carrying agent output from the agent goroutine back into the
// bubbletea update loop. Each is a tea.Msg (which is any); ShellModel.Update
// type-switches on them.

// AssistantMsg is a chunk of model prose.
type AssistantMsg struct{ Text string }

// ToolCallMsg is a tool invocation.
type ToolCallMsg struct{ Name, Args string }

// ToolResultMsg is the outcome of a tool invocation.
type ToolResultMsg struct {
	Name    string
	IsError bool
	Output  string
}

// AgentErrMsg is a non-fatal error surfaced by the agent renderer.
type AgentErrMsg struct{ Err string }

// AgentDoneMsg signals the agent turn finished, carrying the updated history.
type AgentDoneMsg struct{ History []model.Message }

// TokensMsg carries per-turn token counts from the gateway response.
type TokensMsg struct {
	Input  int
	Output int
}

// tickMsg is emitted by the per-second timer while the agent is running.
type tickMsg struct{}

// Compile-time documentation that each value satisfies tea.Msg.
var _ = []tea.Msg{
	AssistantMsg{},
	ToolCallMsg{},
	ToolResultMsg{},
	AgentErrMsg{},
	AgentDoneMsg{},
	TokensMsg{},
	tickMsg{},
}
