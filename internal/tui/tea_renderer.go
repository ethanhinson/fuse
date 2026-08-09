package tui

import (
	"context"
	"fmt"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/tools"
)

// TeaRenderer implements agent.Renderer by forwarding each event as a tea.Msg
// onto a channel that the StartBridges pump delivers into the program. This
// lets the agent run in a goroutine while the UI stays on the event loop.
type TeaRenderer struct{ ch chan<- tea.Msg }

// NewTeaRenderer builds a TeaRenderer that sends onto ch.
func NewTeaRenderer(ch chan<- tea.Msg) *TeaRenderer { return &TeaRenderer{ch: ch} }

// Assistant forwards model prose.
func (r *TeaRenderer) Assistant(text string) { r.ch <- AssistantMsg{Text: text} }

// ToolCall forwards a tool invocation.
func (r *TeaRenderer) ToolCall(name, args string) { r.ch <- ToolCallMsg{Name: name, Args: args} }

// ToolResult forwards a tool outcome.
func (r *TeaRenderer) ToolResult(name string, res tools.Result) {
	r.ch <- ToolResultMsg{Name: name, IsError: res.IsError, Output: res.Output}
}

// Errorf forwards a formatted error line.
func (r *TeaRenderer) Errorf(format string, a ...any) {
	r.ch <- AgentErrMsg{Err: fmt.Sprintf(format, a...)}
}

// Tokens forwards per-turn gateway token counts.
func (r *TeaRenderer) Tokens(input, output int) {
	r.ch <- TokensMsg{Input: input, Output: output}
}

// TeaRenderer satisfies the agent.Renderer interface.
var _ agent.Renderer = (*TeaRenderer)(nil)

// NewTeaAskFunc returns a tools.AskFunc that sends an AskQuestionMsg to ch and
// blocks until the TUI delivers the human's Answer. It mirrors
// NewTeaApprovalFunc: buffered reply channel, ctx-cancellable on both the send
// and the wait, so a turn abort never wedges the agent goroutine.
func NewTeaAskFunc(ch chan<- tea.Msg) tools.AskFunc {
	return func(ctx context.Context, q tools.Question) (tools.Answer, error) {
		respCh := make(chan tools.Answer, 1)
		select {
		case ch <- AskQuestionMsg{Question: q, RespCh: respCh}:
		case <-ctx.Done():
			return tools.Answer{}, ctx.Err()
		}
		select {
		case ans := <-respCh:
			return ans, nil
		case <-ctx.Done():
			return tools.Answer{}, ctx.Err()
		}
	}
}

// NewTeaApprovalFunc returns a permissions.ApprovalFunc that sends a
// PermissionRequestMsg to ch and blocks until the TUI responds.
func NewTeaApprovalFunc(ch chan<- tea.Msg) permissions.ApprovalFunc {
	return func(ctx context.Context, req permissions.ApprovalRequest) (bool, bool, error) {
		respCh := make(chan approvalResponse, 1)
		select {
		case ch <- PermissionRequestMsg{Request: req, RespCh: respCh}:
		case <-ctx.Done():
			return false, false, ctx.Err()
		}
		select {
		case resp := <-respCh:
			return resp.Approved, resp.AllowForSession, nil
		case <-ctx.Done():
			return false, false, ctx.Err()
		}
	}
}
