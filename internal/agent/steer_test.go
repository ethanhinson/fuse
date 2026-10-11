package agent

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/tools"
)

// lastUserText returns the content of the last user message in a request.
func lastUserText(req model.CompletionReq) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			return req.Messages[i].Content
		}
	}
	return ""
}

func TestPollLabelsSteerSource(t *testing.T) {
	bus := NewHumanBus(nil)
	inj := NewHumanInjector("n1", bus)

	// A plain human message keeps the pre-steering text byte for byte.
	bus.Enqueue("n1", ModeDirect, "@human", "hi")
	m, ok := inj.Poll()
	if !ok || m.Content != "[human message]\nhi" {
		t.Fatalf("human poll = %q, %v", m.Content, ok)
	}

	// An orchestrator steer is labelled so the model can tell who is talking.
	bus.Steer("n1", SourceOrchestrator, false, "check the tests first")
	m, ok = inj.Poll()
	if !ok || !strings.HasPrefix(m.Content, "[orchestrator guidance]\n") || !strings.Contains(m.Content, "check the tests first") {
		t.Fatalf("orchestrator poll = %q, %v", m.Content, ok)
	}
	// The model is told whose authority the guidance carries, so it does not mistake it
	// for untrusted injected content.
	if !strings.Contains(m.Content, "speaks for them") {
		t.Fatalf("guidance carries no provenance: %q", m.Content)
	}

	// Mixed sources in one batch keep their own label per segment.
	bus.Enqueue("n1", ModeDirect, "@human", "from human")
	bus.Steer("n1", SourceOrchestrator, false, "from orchestrator")
	m, _ = inj.Poll()
	if !strings.Contains(m.Content, "[human message]\nfrom human") || !strings.Contains(m.Content, "[orchestrator guidance]\n(Sent by the orchestrator") || !strings.Contains(m.Content, "\nfrom orchestrator") {
		t.Fatalf("mixed poll = %q", m.Content)
	}
}

// A steer that arrives while the final (no tool call) model response is being
// produced must not be stranded: the run takes one more turn to answer it.
func TestLateSteerOnFinalTurnIsDelivered(t *testing.T) {
	bus := NewHumanBus(nil)
	inj := NewHumanInjector("n1", bus)

	var mu sync.Mutex
	var seen []string
	calls := 0
	c := completerFunc(func(ctx context.Context, req model.CompletionReq) (model.CompletionResp, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		seen = append(seen, lastUserText(req))
		if calls == 1 {
			// The human steers while the model is writing its final answer.
			bus.Steer("n1", SourceHuman, false, "also update the README")
			return model.CompletionResp{Content: "all done"}, nil
		}
		return model.CompletionResp{Content: "README updated"}, nil
	})
	a := New(c, &fakeExec{}, nopRenderer{}, "m", "", 5, 0)
	a.SetHumanInjector(inj)

	msgs, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "task"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if calls != 2 {
		t.Fatalf("model calls = %d, want 2 (the late steer must get a turn)", calls)
	}
	if !strings.Contains(seen[1], "also update the README") {
		t.Fatalf("second call did not see the steer: %q", seen[1])
	}
	if got := msgs[len(msgs)-1].Content; got != "README updated" {
		t.Fatalf("final message = %q", got)
	}
}

// An interrupt steer cancels the model call that is in flight and the next call
// sees the steer.
func TestInterruptCancelsInFlightModelCall(t *testing.T) {
	bus := NewHumanBus(nil)
	inj := NewHumanInjector("n1", bus)

	started := make(chan struct{})
	var seen []string
	var mu sync.Mutex
	calls := 0
	c := completerFunc(func(ctx context.Context, req model.CompletionReq) (model.CompletionResp, error) {
		mu.Lock()
		calls++
		n := calls
		seen = append(seen, lastUserText(req))
		mu.Unlock()
		if n == 1 {
			close(started)
			<-ctx.Done() // a long generation that only ends when cancelled
			return model.CompletionResp{}, ctx.Err()
		}
		return model.CompletionResp{Content: "changed course"}, nil
	})
	a := New(c, &fakeExec{}, nopRenderer{}, "m", "", 5, 0)
	a.SetHumanInjector(inj)

	done := make(chan error, 1)
	go func() {
		_, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "task"}})
		done <- err
	}()
	<-started
	bus.Steer("n1", SourceOrchestrator, true, "stop, use the other approach")

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("interrupt did not cancel the in-flight model call")
	}
	if !strings.Contains(seen[len(seen)-1], "use the other approach") {
		t.Fatalf("model never saw the steer: %v", seen)
	}
}

// A non-interrupt steer must NOT cancel the in-flight call.
func TestPlainSteerDoesNotInterrupt(t *testing.T) {
	bus := NewHumanBus(nil)
	inj := NewHumanInjector("n1", bus)

	started := make(chan struct{})
	release := make(chan struct{})
	var cancelled bool
	var mu sync.Mutex
	calls := 0
	c := completerFunc(func(ctx context.Context, req model.CompletionReq) (model.CompletionResp, error) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				mu.Lock()
				cancelled = true
				mu.Unlock()
			}
		}
		return model.CompletionResp{Content: "ok"}, nil
	})
	a := New(c, &fakeExec{}, nopRenderer{}, "m", "", 5, 0)
	a.SetHumanInjector(inj)

	done := make(chan struct{})
	go func() {
		_, _ = a.Run(context.Background(), []model.Message{{Role: "user", Content: "task"}})
		close(done)
	}()
	<-started
	bus.Steer("n1", SourceHuman, false, "fyi")
	time.Sleep(50 * time.Millisecond)
	close(release)
	<-done
	mu.Lock()
	defer mu.Unlock()
	if cancelled {
		t.Fatal("a plain steer cancelled the in-flight model call")
	}
}

// An interrupt during a tool batch cancels the running tool, skips the rest with
// well-formed results (provider call/result pairing stays valid), and delivers the steer.
func TestInterruptCancelsToolBatch(t *testing.T) {
	bus := NewHumanBus(nil)
	inj := NewHumanInjector("n1", bus)

	started := make(chan struct{})
	ex := &steerBlockingExec{started: started}
	var seen model.CompletionReq
	calls := 0
	c := completerFunc(func(ctx context.Context, req model.CompletionReq) (model.CompletionResp, error) {
		calls++
		if calls == 1 {
			return model.CompletionResp{ToolCalls: []model.ToolCall{
				{ID: "a", Name: "slow", Arguments: "{}"},
				{ID: "b", Name: "never", Arguments: "{}"},
			}}, nil
		}
		seen = req
		return model.CompletionResp{Content: "re-planned"}, nil
	})
	a := New(c, ex, nopRenderer{}, "m", "", 5, 0)
	a.SetHumanInjector(inj)

	done := make(chan struct{})
	var runErr error
	go func() {
		_, runErr = a.Run(context.Background(), []model.Message{{Role: "user", Content: "task"}})
		close(done)
	}()
	<-started
	bus.Steer("n1", SourceOrchestrator, true, "abort that, do X")
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("interrupt did not cancel the running tool")
	}
	if runErr != nil {
		t.Fatalf("Run: %v", runErr)
	}
	if ex.ran("never") {
		t.Fatal("the remaining tool call ran after the interrupt")
	}
	results := map[string]string{}
	for _, m := range seen.Messages {
		if m.Role == "tool" {
			results[m.ToolCallID] = m.Content
		}
	}
	if len(results) != 2 {
		t.Fatalf("want a result for both tool calls (pairing), got %v", results)
	}
	if !strings.Contains(lastUserText(seen), "abort that, do X") {
		t.Fatalf("steer not delivered after the interrupt: %q", lastUserText(seen))
	}
}

type completerFunc func(context.Context, model.CompletionReq) (model.CompletionResp, error)

func (f completerFunc) Complete(ctx context.Context, req model.CompletionReq) (model.CompletionResp, error) {
	return f(ctx, req)
}

type steerBlockingExec struct {
	started chan struct{}
	once    sync.Once
	mu      sync.Mutex
	names   []string
}

func (e *steerBlockingExec) Schemas() []model.ToolSchema { return nil }
func (e *steerBlockingExec) Execute(ctx context.Context, name, args string) tools.Result {
	e.mu.Lock()
	e.names = append(e.names, name)
	e.mu.Unlock()
	if name == "slow" {
		e.once.Do(func() { close(e.started) })
		<-ctx.Done()
		return tools.Result{Output: "cancelled", IsError: true}
	}
	return tools.Result{Output: "ok"}
}
func (e *steerBlockingExec) ran(name string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, n := range e.names {
		if n == name {
			return true
		}
	}
	return false
}
