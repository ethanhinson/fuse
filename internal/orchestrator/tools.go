package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/tools"
)

type statusTool struct{ w *watcher }

func (t *statusTool) Name() string { return "loop_status" }
func (t *statusTool) Description() string {
	return "Show what the agent you are watching is doing right now: its current state and its most recent activity (user turns, model replies, tool calls and results, sub-agents, errors). Call this before advising or steering."
}
func (t *statusTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"limit": map[string]any{"type": "integer", "description": "How many recent events to show (default 25)."},
		},
	}
}
func (t *statusTool) Execute(ctx context.Context, args string) tools.Result {
	var in struct {
		Limit int `json:"limit"`
	}
	if strings.TrimSpace(args) != "" {
		if err := json.Unmarshal([]byte(args), &in); err != nil {
			return tools.Result{Output: "bad arguments: " + err.Error(), IsError: true}
		}
	}
	if in.Limit <= 0 {
		in.Limit = defaultLimit
	}
	if err := t.w.refresh(ctx); err != nil {
		return tools.Result{Output: "could not read the agent's activity: " + err.Error(), IsError: true}
	}
	t.w.mu.Lock()
	defer t.w.mu.Unlock()
	var lines []string
	for i := len(t.w.ring) - 1; i >= 0 && len(lines) < in.Limit; i-- {
		if t.w.ring[i].line != "" {
			lines = append(lines, t.w.ring[i].line)
		}
	}
	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
		lines[i], lines[j] = lines[j], lines[i]
	}
	out := "state: " + state(t.w.ring) + "\n"
	if len(lines) == 0 {
		out += "no recent activity"
	} else {
		out += "recent activity (oldest first):\n" + strings.Join(lines, "\n")
	}
	return tools.Result{Output: capOutput(out)}
}

type eventTool struct{ w *watcher }

func (t *eventTool) Name() string { return "loop_event" }
func (t *eventTool) Description() string {
	return "Show one event from the watched agent in full, by its #seq number from loop_status. Use it to read a whole tool result or model reply."
}
func (t *eventTool) Parameters() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"seq"},
		"properties": map[string]any{
			"seq": map[string]any{"type": "integer", "description": "The event number shown as #N in loop_status."},
		},
	}
}
func (t *eventTool) Execute(ctx context.Context, args string) tools.Result {
	var in struct {
		Seq event.Seq `json:"seq"`
	}
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return tools.Result{Output: "bad arguments: " + err.Error(), IsError: true}
	}
	if err := t.w.refresh(ctx); err != nil {
		return tools.Result{Output: "could not read the agent's activity: " + err.Error(), IsError: true}
	}
	t.w.mu.Lock()
	defer t.w.mu.Unlock()
	for _, en := range t.w.ring {
		e := en.ev
		if e.Seq != in.Seq {
			continue
		}
		var pretty strings.Builder
		pretty.WriteString(fmt.Sprintf("#%d %s (turn %d)\n", e.Seq, e.Kind, e.Turn))
		if len(e.Payload) == 0 {
			pretty.WriteString("(payload was too large to keep; the one-line summary is: " + en.line + ")")
		} else {
			var v any
			if json.Unmarshal(e.Payload, &v) == nil {
				b, _ := json.MarshalIndent(v, "", "  ")
				pretty.Write(b)
			} else {
				pretty.Write(e.Payload)
			}
		}
		return tools.Result{Output: capOutput(pretty.String())}
	}
	return tools.Result{Output: fmt.Sprintf("no event #%d among the recent %d events", in.Seq, ringSize), IsError: true}
}

type steerTool struct{ t Target }

func (t *steerTool) Name() string { return "steer_agent" }
func (t *steerTool) Description() string {
	return "Send guidance to the agent you are watching. It reads the message as an instruction from the orchestrator. Set interrupt only when the agent must stop what it is doing right now (wrong direction, about to do harm, wasting effort); otherwise the message is delivered at the agent's next step."
}
func (t *steerTool) Parameters() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"message"},
		"properties": map[string]any{
			"message":   map[string]any{"type": "string", "description": "What to tell the agent. Be specific and self-contained."},
			"interrupt": map[string]any{"type": "boolean", "description": "Cancel the agent's current model call or tool run so the message lands immediately."},
		},
	}
}
func (t *steerTool) Execute(ctx context.Context, args string) tools.Result {
	var in struct {
		Message   string `json:"message"`
		Interrupt bool   `json:"interrupt"`
	}
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return tools.Result{Output: "bad arguments: " + err.Error(), IsError: true}
	}
	if strings.TrimSpace(in.Message) == "" {
		return tools.Result{Output: "message is empty", IsError: true}
	}
	if err := t.t.Steer(ctx, in.Message, in.Interrupt); err != nil {
		return tools.Result{Output: "could not steer the agent: " + err.Error(), IsError: true}
	}
	if in.Interrupt {
		return tools.Result{Output: "sent. The agent was interrupted and will read your message now."}
	}
	return tools.Result{Output: "sent. The agent will read it at its next step."}
}
