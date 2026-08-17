package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/tools"
)

// denyExec is a ToolExecutor whose scripted outcomes drive the change-0067
// policy-denial protocol: each Execute pops the next outcome (true = a typed
// policy denial on the given layer, false = success). Exhausted ⇒ success.
type denyExec struct {
	layer   string
	denials []bool
	calls   int
}

func (d *denyExec) Schemas() []model.ToolSchema { return nil }
func (d *denyExec) Execute(_ context.Context, name, _ string) tools.Result {
	i := d.calls
	d.calls++
	if i < len(d.denials) && d.denials[i] {
		return tools.Result{
			IsError:   true,
			Output:    "denied by auto-mode " + d.layer + " layer: " + name + "; retrying the identical call will fail",
			Denied:    true,
			DenyLayer: d.layer,
		}
	}
	return tools.Result{Output: "ok"}
}

// TestDeniedRepeat_NudgeInjectedThenHeadlessAbort drives the full headless
// protocol: deny → denied repeat (no generic loop trip) → second denial
// injects the [policy] nudge as a user turn AND emits it as user.input →
// post-nudge repeat ends the run with ErrLoopDetected naming the nudge.
func TestDeniedRepeat_NudgeInjectedThenHeadlessAbort(t *testing.T) {
	same := model.CompletionResp{ToolCalls: []model.ToolCall{{ID: "x", Name: "bash", Arguments: `{"command":"curl x"}`}}}
	comp := &scriptedCompleter{responses: []model.CompletionResp{same, same, same}}
	exec := &denyExec{layer: "rules", denials: []bool{true, true, true}}
	rec := &recordingStore{}
	a := New(comp, exec, nopRenderer{}, "m", "", 50, 100) // no LoopApproval: headless
	a.SetEventSink(rec)

	hist, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "hi"}})
	if !errors.Is(err, ErrLoopDetected) {
		t.Fatalf("post-nudge repeat must abort with ErrLoopDetected, got %v", err)
	}
	if !strings.Contains(err.Error(), "policy-denial nudge") {
		t.Errorf("abort should name the nudge protocol, got %q", err.Error())
	}
	// Both denials executed; the killed third turn never dispatched.
	if exec.calls != 2 {
		t.Errorf("expected 2 executions (third repeat aborts before dispatch), got %d", exec.calls)
	}
	// The nudge is a real user turn in the transcript…
	var nudge string
	for _, m := range hist {
		if m.Role == "user" && strings.HasPrefix(m.Content, "[policy]") {
			nudge = m.Content
		}
	}
	if nudge == "" {
		t.Fatal("no [policy] nudge user turn in the transcript")
	}
	if !strings.Contains(nudge, "rules") || !strings.Contains(nudge, "bash") {
		t.Errorf("nudge should name the tool and layer, got %q", nudge)
	}
	// …and emitted as user.input so change-0054 resume folds stay faithful.
	found := false
	for _, e := range rec.events {
		if e.Kind == event.KindUserInput && strings.Contains(string(e.Payload), "[policy]") {
			found = true
		}
	}
	if !found {
		t.Error("nudge was not emitted as a user.input event")
	}
	// The trip event carries the policy reason.
	tripped := false
	for _, e := range rec.events {
		if e.Kind == event.KindLoopTrip && strings.Contains(string(e.Payload), "policy-denied") {
			tripped = true
		}
	}
	if !tripped {
		t.Error("loop.detector.trip should carry the policy-denial reason")
	}
}

// TestDeniedRepeat_InteractivePromptContinues proves the interactive arm: a
// post-nudge repeat consults LoopApproval with a policy-aware preview, and
// approval clears the trackers so the run continues.
func TestDeniedRepeat_InteractivePromptContinues(t *testing.T) {
	same := model.CompletionResp{ToolCalls: []model.ToolCall{{ID: "x", Name: "bash", Arguments: `{"command":"curl x"}`}}}
	comp := &scriptedCompleter{responses: []model.CompletionResp{same, same, same, {Content: "done"}}}
	exec := &denyExec{layer: "rules", denials: []bool{true, true, true, true}}
	a := New(comp, exec, nopRenderer{}, "m", "", 50, 100)
	var previews []string
	a.LoopApproval = func(_ context.Context, preview string) (bool, error) {
		previews = append(previews, preview)
		return true, nil
	}
	_, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("approved policy-repeat should continue to the natural stop, got %v", err)
	}
	if len(previews) != 1 || !strings.Contains(previews[0], "policy-blocked") {
		t.Fatalf("expected one policy-aware approval preview, got %v", previews)
	}
}

// TestDeniedThenSuccess_ClearsTracker proves a denial followed by a successful
// identical call resets its budget: no nudge, no abort, clean finish.
func TestDeniedThenSuccess_ClearsTracker(t *testing.T) {
	same := model.CompletionResp{ToolCalls: []model.ToolCall{{ID: "x", Name: "bash", Arguments: `{"command":"make"}`}}}
	comp := &scriptedCompleter{responses: []model.CompletionResp{same, same, {Content: "done"}}}
	exec := &denyExec{layer: "classifier", denials: []bool{true, false}}
	a := New(comp, exec, nopRenderer{}, "m", "", 50, 100)
	hist, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("denial then success must finish cleanly, got %v", err)
	}
	for _, m := range hist {
		if strings.HasPrefix(m.Content, "[policy]") {
			t.Fatalf("no nudge should be injected after a single denial, found %q", m.Content)
		}
	}
}

// TestValveDenial_HeadlessReturnsErrAutoModePaused proves a valve-layer denial
// in a headless run ends it with one structured stop, tool-result pairing
// intact, instead of cascading further denials.
func TestValveDenial_HeadlessReturnsErrAutoModePaused(t *testing.T) {
	call := model.CompletionResp{ToolCalls: []model.ToolCall{{ID: "x", Name: "bash", Arguments: `{"command":"foo"}`}}}
	comp := &scriptedCompleter{responses: []model.CompletionResp{call, call}}
	exec := &denyExec{layer: permissions.LayerValve, denials: []bool{true, true}}
	rec := &recordingStore{}
	a := New(comp, exec, nopRenderer{}, "m", "", 50, 100)
	a.SetEventSink(rec)

	hist, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "hi"}})
	if !errors.Is(err, ErrAutoModePaused) {
		t.Fatalf("expected ErrAutoModePaused, got %v", err)
	}
	if exec.calls != 1 {
		t.Errorf("run must stop after the first valve denial, got %d executions", exec.calls)
	}
	last := hist[len(hist)-1]
	if last.Role != "tool" || last.ToolCallID != "x" {
		t.Errorf("tool-result pairing must stay intact on the early return, last = %+v", last)
	}
	errored := false
	for _, e := range rec.events {
		if e.Kind == event.KindError && strings.Contains(string(e.Payload), "auto mode paused") {
			errored = true
		}
	}
	if !errored {
		t.Error("valve pause should emit a structured error event")
	}
}

// TestPermissionDecisionEmitted is the end-to-end seam test: a real
// PermissionGate as the executor, the loop's per-call sink, and the event
// store. One auto-approved bash call must yield a permission.decision event
// stamped with the tool-call ID.
func TestPermissionDecisionEmitted(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(stubBashTool{})
	gate := permissions.New(config.PermissionsConfig{Mode: "auto"}, reg, permissions.AlwaysApprove,
		permissions.WithWorkspaceRoot(t.TempDir()))

	comp := &scriptedCompleter{responses: []model.CompletionResp{
		{ToolCalls: []model.ToolCall{{ID: "call-7", Name: "bash", Arguments: `{"command":"git status"}`}}},
		{Content: "done"},
	}}
	rec := &recordingStore{}
	a := New(comp, gate, nopRenderer{}, "m", "", 10, 100)
	a.SetEventSink(rec)

	if _, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatal(err)
	}
	var decision *event.Event
	for i, e := range rec.events {
		if e.Kind == event.KindPermissionDecision {
			decision = &rec.events[i]
		}
	}
	if decision == nil {
		t.Fatal("no permission.decision event emitted")
	}
	p := string(decision.Payload)
	for _, want := range []string{`"tool_call_id":"call-7"`, `"verdict":"allow"`, `"mode":"auto"`, `"command":"git status"`} {
		if !strings.Contains(p, want) {
			t.Errorf("decision payload missing %s: %s", want, p)
		}
	}
}

// stubBashTool satisfies tools.Tool for the registry behind the gate.
type stubBashTool struct{}

func (stubBashTool) Name() string               { return "bash" }
func (stubBashTool) Description() string        { return "stub" }
func (stubBashTool) Parameters() map[string]any { return map[string]any{} }
func (stubBashTool) Execute(context.Context, string) tools.Result {
	return tools.Result{Output: "ok"}
}
