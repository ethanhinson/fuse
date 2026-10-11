package orchestrator

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/tools"
)

type fakeTarget struct {
	mu     sync.Mutex
	events []event.Event
	steers []steerCall
	err    error
}

type steerCall struct {
	text      string
	interrupt bool
}

func (f *fakeTarget) Events(_ context.Context, after event.Seq) ([]event.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []event.Event
	for _, e := range f.events {
		if e.Seq > after {
			out = append(out, e)
		}
	}
	return out, nil
}

func (f *fakeTarget) Steer(_ context.Context, text string, interrupt bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.steers = append(f.steers, steerCall{text, interrupt})
	return nil
}

func (f *fakeTarget) add(kind event.Kind, payload any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, _ := json.Marshal(payload)
	f.events = append(f.events, event.Event{Seq: event.Seq(len(f.events) + 1), Kind: kind, NodeID: "root", Payload: raw})
}

func call(t *testing.T, reg *tools.Registry, name, args string) tools.Result {
	t.Helper()
	if !reg.Has(name) {
		t.Fatalf("tool %q not registered", name)
	}
	return reg.Execute(context.Background(), name, args)
}

func TestStatusShowsRunningToolAndRecentActivity(t *testing.T) {
	ft := &fakeTarget{}
	ft.add(event.KindUserInput, event.UserInputPayload{Content: "fix the flaky test"})
	ft.add(event.KindModelCallEnd, event.ModelCallEndPayload{Content: "looking at it", ToolCalls: []event.ToolCallRef{{ID: "1", Name: "bash"}}})
	ft.add(event.KindToolCall, event.ToolCallPayload{ID: "1", Name: "bash", Args: json.RawMessage(`{"command":"go test ./..."}`)})

	reg := tools.NewRegistry()
	Register(reg, ft)
	res := call(t, reg, "loop_status", `{}`)
	if res.IsError {
		t.Fatalf("loop_status error: %s", res.Output)
	}
	for _, want := range []string{"running tool bash", "fix the flaky test", "looking at it", "go test ./..."} {
		if !strings.Contains(res.Output, want) {
			t.Errorf("status missing %q in:\n%s", want, res.Output)
		}
	}

	// Once the tool returns and the loop parks, the state changes.
	ft.add(event.KindToolResult, event.ToolResultPayload{ID: "1", Name: "bash", Result: "FAIL"})
	ft.add(event.KindLoopParked, event.LoopParkedPayload{Content: "it still fails"})
	res = call(t, reg, "loop_status", `{}`)
	if !strings.Contains(res.Output, "idle") || strings.Contains(res.Output, "running tool bash") {
		t.Errorf("want idle state after park:\n%s", res.Output)
	}
}

func TestStatusIsIncrementalAndBounded(t *testing.T) {
	ft := &fakeTarget{}
	for i := 0; i < 500; i++ {
		ft.add(event.KindToolResult, event.ToolResultPayload{ID: "x", Name: "read_file", Result: strings.Repeat("z", 5000)})
	}
	reg := tools.NewRegistry()
	Register(reg, ft)
	res := call(t, reg, "loop_status", `{"limit":5}`)
	if got := strings.Count(res.Output, "tool.result"); got != 5 {
		t.Fatalf("want 5 recent events, got %d", got)
	}
	if len(res.Output) > 8000 {
		t.Fatalf("status output unbounded: %d bytes", len(res.Output))
	}
}

func TestEventReturnsFullPayload(t *testing.T) {
	ft := &fakeTarget{}
	ft.add(event.KindToolResult, event.ToolResultPayload{ID: "1", Name: "bash", Result: "line one\nline two"})
	reg := tools.NewRegistry()
	Register(reg, ft)
	res := call(t, reg, "loop_event", `{"seq":1}`)
	if res.IsError || !strings.Contains(res.Output, "line two") {
		t.Fatalf("loop_event = %+v", res)
	}
	if res := call(t, reg, "loop_event", `{"seq":99}`); !res.IsError {
		t.Fatalf("unknown seq should error")
	}
}

func TestSteerAgentForwardsAndValidates(t *testing.T) {
	ft := &fakeTarget{}
	reg := tools.NewRegistry()
	Register(reg, ft)

	res := call(t, reg, "steer_agent", `{"message":"use the other approach","interrupt":true}`)
	if res.IsError {
		t.Fatalf("steer_agent: %s", res.Output)
	}
	if len(ft.steers) != 1 || ft.steers[0] != (steerCall{"use the other approach", true}) {
		t.Fatalf("steers = %+v", ft.steers)
	}
	if !strings.Contains(res.Output, "interrupt") {
		t.Errorf("result should say the agent was interrupted: %s", res.Output)
	}

	if res := call(t, reg, "steer_agent", `{"message":"  "}`); !res.IsError {
		t.Fatal("empty message must be rejected")
	}
	if res := call(t, reg, "steer_agent", `not json`); !res.IsError {
		t.Fatal("bad args must be rejected")
	}

	ft.err = context.Canceled
	if res := call(t, reg, "steer_agent", `{"message":"x"}`); !res.IsError {
		t.Fatal("a failed steer must surface as an error result")
	}
}

func TestSystemPromptNamesTheTools(t *testing.T) {
	p := SystemPrompt()
	for _, n := range []string{"loop_status", "loop_event", "steer_agent"} {
		if !strings.Contains(p, n) {
			t.Errorf("system prompt does not mention %s", n)
		}
	}
}

func TestHugePayloadStillShowsInStatus(t *testing.T) {
	ft := &fakeTarget{}
	ft.add(event.KindToolResult, event.ToolResultPayload{ID: "1", Name: "read_file", Result: strings.Repeat("a", 40<<10)})
	reg := tools.NewRegistry()
	Register(reg, ft)
	res := call(t, reg, "loop_status", `{}`)
	if !strings.Contains(res.Output, "read_file ok") {
		t.Fatalf("a huge result vanished from status:\n%s", res.Output)
	}
}
