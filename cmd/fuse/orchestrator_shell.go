package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/orchestrator"
	"github.com/ethanhinson/fuse/internal/tools"
	"github.com/ethanhinson/fuse/internal/tui"
)

// shellOrchestrator is the interactive shell's /orchestrator chat. It runs one
// persistent orchestrator loop (an interactive agent that parks between messages) on
// its own goroutine. The person's words go in through the loop's own human bus; what
// the orchestrator says comes back as tui messages on the shell's channel. It starts on
// the first Say, so a session that never opens /orchestrator pays nothing.
type shellOrchestrator struct {
	ctx    context.Context
	out    chan<- tea.Msg
	target orchestrator.Target
	// newAgent builds the orchestrator agent over the given renderer and tool registry.
	// The registry holds only the orchestrator tools; the agent gates it like any other.
	newAgent func(r agent.Renderer, reg *tools.Registry) (*agent.Agent, error)

	bus  *agent.HumanBus
	node string

	mu      sync.Mutex
	started bool

	// parked is signalled each time the orchestrator finishes an answer and waits for
	// the next message. The shell ignores it; a line-by-line client uses WaitIdle to
	// know when to prompt again.
	parked chan struct{}
}

const orchestratorNode = "orchestrator"

func newShellOrchestrator(ctx context.Context, out chan<- tea.Msg, target orchestrator.Target,
	newAgent func(r agent.Renderer, reg *tools.Registry) (*agent.Agent, error)) *shellOrchestrator {
	return &shellOrchestrator{
		ctx: ctx, out: out, target: target, newAgent: newAgent,
		bus: agent.NewHumanBus(nil), node: orchestratorNode,
		parked: make(chan struct{}, 1),
	}
}

// Say queues the person's message and starts the loop on first use. The message is
// queued BEFORE the loop starts so its first turn already holds it: an interactive loop
// that began with an empty conversation would send the model nothing to answer.
func (o *shellOrchestrator) Say(text string) {
	o.bus.Enqueue(o.node, agent.ModeDirect, "@you", text)
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.started {
		o.started = o.start()
	}
}

// start builds and launches the loop, reporting whether it is now running. A failed
// build leaves the session unstarted so the next Say tries again.
func (o *shellOrchestrator) start() bool {
	reg := tools.NewRegistry()
	orchestrator.Register(reg, o.target)
	r := &orchestratorRenderer{ctx: o.ctx, out: o.out}
	a, err := o.newAgent(r, reg)
	if err != nil {
		o.send(tui.OrchErrMsg{Err: "could not start the orchestrator: " + err.Error()})
		return false
	}
	a.SetSystemPrompt(orchestrator.SystemPrompt())
	a.SetInteractive(true)
	a.SetMaxTurns(0) // a conversation must not end at a turn cap
	a.SetHumanInjector(agent.NewHumanInjector(o.node, o.bus))
	a.SetEventSink(parkSink{onParked: func() {
		select {
		case o.parked <- struct{}{}:
		default: // a signal is already pending
		}
	}})
	go func() {
		if _, err := a.Run(o.ctx, nil); err != nil && o.ctx.Err() == nil {
			o.send(tui.OrchErrMsg{Err: err.Error()})
		}
	}()
	return true
}

// WaitIdle blocks until the orchestrator has finished answering and is waiting for the
// next message, or ctx is done.
func (o *shellOrchestrator) WaitIdle(ctx context.Context) error {
	select {
	case <-o.parked:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// parkSink is an event sink that only watches for the loop parking.
type parkSink struct{ onParked func() }

func (p parkSink) Append(e event.Event) error {
	if e.Kind == event.KindLoopParked {
		p.onParked()
	}
	return nil
}

func (parkSink) Subscribe() (<-chan event.Event, func()) {
	c := make(chan event.Event)
	close(c)
	return c, func() {}
}

func (parkSink) Replay(event.Seq) ([]event.Event, error) { return nil, nil }

func (o *shellOrchestrator) send(m tea.Msg) {
	select {
	case o.out <- m:
	case <-o.ctx.Done():
	}
}

// orchestratorRenderer turns the orchestrator agent's output into shell messages: its
// words become replies, its steering becomes a one-line action, and its failures become
// errors. Looking at the agent is deliberately quiet.
type orchestratorRenderer struct {
	ctx context.Context
	out chan<- tea.Msg
}

func (r *orchestratorRenderer) send(m tea.Msg) {
	select {
	case r.out <- m:
	case <-r.ctx.Done():
	}
}

func (r *orchestratorRenderer) Assistant(text string) {
	if strings.TrimSpace(text) != "" {
		r.send(tui.OrchReplyMsg{Text: text})
	}
}

func (r *orchestratorRenderer) ToolCall(name, args string) {
	if name != "steer_agent" {
		return
	}
	var in struct {
		Message   string `json:"message"`
		Interrupt bool   `json:"interrupt"`
	}
	_ = json.Unmarshal([]byte(args), &in)
	verb := "steering the agent"
	if in.Interrupt {
		verb = "interrupting the agent"
	}
	r.send(tui.OrchActionMsg{Text: fmt.Sprintf("%s: %s", verb, in.Message)})
}

func (r *orchestratorRenderer) ToolResult(name string, res tools.Result) {
	if res.IsError {
		r.send(tui.OrchErrMsg{Err: name + ": " + res.Output})
	}
}

func (r *orchestratorRenderer) Errorf(format string, a ...any) {
	r.send(tui.OrchErrMsg{Err: fmt.Sprintf(format, a...)})
}

func (r *orchestratorRenderer) Tokens(int, int) {}
