// Package probe provides an observability harness for watching what fuse
// actually does during a run — every agent's assistant text, tool call, tool
// result, and token count, attributed to the agent that produced it, collected
// into one structured, inspectable log.
//
// It exists because the interesting fuse behaviors (the /research fan-out in
// particular) are emergent: the root diversifies a question, spawns one
// subagent per facet, and each child searches and fetches on its own. None of
// that is visible from a unit test that fakes the model, and the interactive
// TUI scrolls it past too fast to inspect. A Recorder is an agent.Renderer, so
// it drops straight into the same seam the TUI renderer uses — wrap the real
// renderer with tui.NewMultiRenderer(real, recorder), or use a Recorder alone
// in a headless run — and afterwards Log.Report() prints a readable transcript:
// the spawn tree, a tool-call census (searches with queries, fetches with
// URLs), and the final synthesized output.
package probe

import (
	"sync"
	"time"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/tools"
)

// EventKind classifies a recorded event.
type EventKind string

const (
	KindAssistant  EventKind = "assistant"
	KindToolCall   EventKind = "tool_call"
	KindToolResult EventKind = "tool_result"
	KindError      EventKind = "error"
	KindTokens     EventKind = "tokens"
)

// Event is one thing that happened during a run, attributed to the agent node
// (by label) that produced it. Events are captured in wall-clock order across
// all agents, so the interleaving of a parent and its concurrent children is
// preserved exactly as it occurred.
type Event struct {
	Seq     int
	At      time.Time
	Node    string // agent label, e.g. "root" or a facet label
	Kind    EventKind
	Name    string // tool name (KindToolCall / KindToolResult)
	Text    string // assistant prose, tool args, tool output, or error text
	IsError bool   // tool result was an error
	In, Out int    // token counts (KindTokens)
}

// Log is the shared, thread-safe sink every per-node Recorder writes into. One
// Log backs a whole run; each agent gets its own Recorder pointing at it, so
// concurrent children never race on the slice.
type Log struct {
	mu     sync.Mutex
	events []Event
	seq    int
	now    func() time.Time // injectable clock for deterministic tests
}

// NewLog returns an empty Log using the wall clock.
func NewLog() *Log { return &Log{now: time.Now} }

// WithClock overrides the clock (tests want a deterministic timestamp).
func (l *Log) WithClock(now func() time.Time) *Log {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.now = now
	return l
}

// Recorder returns an agent.Renderer that attributes every event it receives to
// node and appends it to this Log. Give each agent (root and every child) its
// own Recorder with that agent's label.
func (l *Log) Recorder(node string) *Recorder { return &Recorder{log: l, node: node} }

func (l *Log) append(e Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	e.Seq = l.seq
	if l.now != nil {
		e.At = l.now()
	}
	l.events = append(l.events, e)
}

// Events returns a copy of every recorded event in capture order.
func (l *Log) Events() []Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Event, len(l.events))
	copy(out, l.events)
	return out
}

// Recorder is a per-agent agent.Renderer that forwards each event to a shared
// Log, tagged with this agent's node label. It is safe for concurrent use: a
// single Recorder is only ever called from one agent's loop, and the Log it
// writes to is mutex-guarded, so many Recorders may write at once.
type Recorder struct {
	log  *Log
	node string
}

// Assistant records a chunk of model prose.
func (r *Recorder) Assistant(text string) {
	r.log.append(Event{Node: r.node, Kind: KindAssistant, Text: text})
}

// ToolCall records a tool invocation and its raw JSON arguments.
func (r *Recorder) ToolCall(name, args string) {
	r.log.append(Event{Node: r.node, Kind: KindToolCall, Name: name, Text: args})
}

// ToolResult records a tool's outcome.
func (r *Recorder) ToolResult(name string, res tools.Result) {
	r.log.append(Event{Node: r.node, Kind: KindToolResult, Name: name, Text: res.Output, IsError: res.IsError})
}

// Errorf records a formatted error line.
func (r *Recorder) Errorf(format string, a ...any) {
	r.log.append(Event{Node: r.node, Kind: KindError, Text: sprintf(format, a...)})
}

// Tokens records per-turn gateway token counts.
func (r *Recorder) Tokens(input, output int) {
	r.log.append(Event{Node: r.node, Kind: KindTokens, In: input, Out: output})
}

// Recorder satisfies agent.Renderer.
var _ agent.Renderer = (*Recorder)(nil)
