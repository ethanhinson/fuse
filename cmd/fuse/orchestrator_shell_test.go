package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/orchestrator"
	"github.com/ethanhinson/fuse/internal/tools"
	"github.com/ethanhinson/fuse/internal/tui"
)

type scriptedOrch struct {
	mu    sync.Mutex
	steps []model.CompletionResp
	n     int
	seen  []model.CompletionReq
}

func (s *scriptedOrch) Complete(ctx context.Context, req model.CompletionReq) (model.CompletionResp, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = append(s.seen, req)
	if s.n < len(s.steps) {
		r := s.steps[s.n]
		s.n++
		return r, nil
	}
	return model.CompletionResp{Content: "ok"}, nil
}

type regExec struct{ reg *tools.Registry }

func (e regExec) Schemas() []model.ToolSchema { return e.reg.Schemas() }
func (e regExec) Execute(ctx context.Context, name, args string) tools.Result {
	return e.reg.Execute(ctx, name, args)
}

type memEvents struct{}

func (memEvents) Append(event.Event) error { return nil }
func (memEvents) Subscribe() (<-chan event.Event, func()) {
	c := make(chan event.Event)
	close(c)
	return c, func() {}
}
func (memEvents) Replay(event.Seq) ([]event.Event, error) { return nil, nil }

// The shell orchestrator answers the person and steers the working agent through its
// human bus, with the orchestrator label the agent will see.
func TestShellOrchestratorChatsAndSteers(t *testing.T) {
	workBus := agent.NewHumanBus(nil)
	target := &orchestrator.LocalTarget{Store: memEvents{}, Bus: workBus, NodeID: "root"}

	fake := &scriptedOrch{steps: []model.CompletionResp{
		{ToolCalls: []model.ToolCall{{ID: "1", Name: "steer_agent", Arguments: `{"message":"run the tests first","interrupt":true}`}}},
		{Content: "I told it to run the tests first."},
	}}
	out := make(chan tea.Msg, 16)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	o := newShellOrchestrator(ctx, out, target, func(r agent.Renderer, reg *tools.Registry) (*agent.Agent, error) {
		return agent.New(fake, regExec{reg}, r, "orch-model", "", 1, 0), nil
	})
	var _ tui.OrchestratorSession = o

	o.Say("make it run the tests before finishing")

	var sawAction, sawReply bool
	deadline := time.After(3 * time.Second)
	for !(sawAction && sawReply) {
		select {
		case m := <-out:
			switch v := m.(type) {
			case tui.OrchActionMsg:
				sawAction = strings.Contains(v.Text, "interrupting the agent") && strings.Contains(v.Text, "run the tests first")
			case tui.OrchReplyMsg:
				sawReply = strings.Contains(v.Text, "told it")
			case tui.OrchErrMsg:
				t.Fatalf("orchestrator error: %s", v.Err)
			}
		case <-deadline:
			t.Fatalf("timed out (action=%v reply=%v)", sawAction, sawReply)
		}
	}

	msg, ok := agent.NewHumanInjector("root", workBus).Poll()
	if !ok || !strings.Contains(msg.Content, "[orchestrator guidance]") || !strings.Contains(msg.Content, "run the tests first") {
		t.Fatalf("working agent got %q ok=%v", msg.Content, ok)
	}

	// The first request carried the person's words and the orchestrator prompt; it did
	// not start from an empty conversation.
	fake.mu.Lock()
	first := fake.seen[0]
	fake.mu.Unlock()
	var sys, user string
	for _, m := range first.Messages {
		switch m.Role {
		case "system":
			sys = m.Content
		case "user":
			user = m.Content
		}
	}
	if !strings.Contains(sys, "orchestrator") || !strings.Contains(user, "make it run the tests") {
		t.Fatalf("first request: system=%q user=%q", sys, user)
	}
}

// A second message reuses the same running loop (one conversation).
func TestShellOrchestratorKeepsOneConversation(t *testing.T) {
	fake := &scriptedOrch{steps: []model.CompletionResp{{Content: "first"}, {Content: "second"}}}
	out := make(chan tea.Msg, 16)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	builds := 0
	o := newShellOrchestrator(ctx, out, &orchestrator.LocalTarget{Store: memEvents{}, Bus: agent.NewHumanBus(nil), NodeID: "root"},
		func(r agent.Renderer, reg *tools.Registry) (*agent.Agent, error) {
			builds++
			return agent.New(fake, regExec{reg}, r, "m", "", 1, 0), nil
		})

	o.Say("hello")
	waitReply(t, out, "first")
	o.Say("and again")
	waitReply(t, out, "second")
	if builds != 1 {
		t.Fatalf("agent built %d times, want 1", builds)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if n := len(fake.seen[1].Messages); n < 3 {
		t.Fatalf("second turn lost the conversation (%d messages)", n)
	}
}

func waitReply(t *testing.T, out <-chan tea.Msg, want string) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case m := <-out:
			if r, ok := m.(tui.OrchReplyMsg); ok && r.Text == want {
				return
			}
		case <-deadline:
			t.Fatalf("no reply %q", want)
		}
	}
}
