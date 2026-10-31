package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/runtime"
	"github.com/ethanhinson/fuse/internal/tools/sandbox"
)

// TestHostedBindingDeniesExplicitAskRule proves, at the composition root the
// hosted loop servers share (buildGate under markPolicyApproval + AlwaysApprove),
// that an explicit auto.ask rule is denied at the rules layer instead of being
// allowed by the safelist or auto-approved by the stand-in, and that the command
// never ran.
func TestHostedBindingDeniesExplicitAskRule(t *testing.T) {
	prev := gateApprovalProvenance
	t.Cleanup(func() { gateApprovalProvenance = prev })
	markPolicyApproval()

	cfg := config.Config{Permissions: config.PermissionsConfig{
		Mode: "auto",
		Auto: config.AutoConfig{Ask: []string{"bash:touch*"}},
	}}
	svc, err := sandbox.NewService(sandbox.Config{Contained: false, Handler: sandbox.HandlerHost})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	toolReg := defaultToolRegistry(svc, cfg.Research, nil)
	gate := buildGate(cfg, toolReg, permissions.AlwaysApprove, nil, nil, nil)

	var decisions []permissions.Decision
	ctx := permissions.WithDecisionSink(context.Background(), func(d permissions.Decision) {
		decisions = append(decisions, d)
	})
	// Relative sentinel: path.Match's * does not cross "/", so the pattern
	// bash:touch* only matches a slash-free argv.
	dir := t.TempDir()
	t.Chdir(dir)
	sentinel := filepath.Join(dir, "ran")
	res := gate.Execute(ctx, "bash", `{"command":"touch ran"}`)
	if !res.IsError || !res.Denied {
		t.Fatalf("explicit ask under the hosted binding must deny; res=%+v decisions=%+v", res, decisions)
	}
	if _, err := os.Stat(sentinel); err == nil {
		t.Fatalf("denied command was executed: %s exists", sentinel)
	}
	var ok bool
	for _, d := range decisions {
		if d.Verdict == "deny" && d.Layer == permissions.LayerRules && strings.Contains(d.Reason, "bash:touch*") {
			ok = true
		}
	}
	if !ok {
		t.Fatalf("want permission.decision deny/rules naming the pattern; got %+v", decisions)
	}
}

// TestLoopServerDeniesExplicitAskRuleEndToEnd runs the production loop-server
// runtime against a scripted gateway whose model asks for a command matching an
// operator auto.ask rule. With no human bound the call must be denied at the
// rules layer, visible in the durable event stream. The harness binds no
// sandbox, so bash could not run here either way; the decision event is the
// proof, and TestHostedBindingDeniesExplicitAskRule covers non-execution on a real one.
func TestLoopServerDeniesExplicitAskRuleEndToEnd(t *testing.T) {
	prev := gateApprovalProvenance
	t.Cleanup(func() { gateApprovalProvenance = prev })
	markPolicyApproval()

	args, _ := json.Marshal(map[string]string{"command": "touch ran"})
	callArgs, _ := json.Marshal(string(args))
	toolCall := `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"bash","arguments":` + string(callArgs) + `}}]}}]}`
	final := `{"choices":[{"message":{"role":"assistant","content":"done"}}]}`

	rt, gw, _ := harnessServer(t, harnessOperator+`
permissions:
  mode: auto
  auto:
    ask: ["bash:touch*"]
`)
	gw.script = []string{toolCall, final}

	h, err := rt.StartLoop(context.Background(), runtime.LoopConfig{Task: "go"})
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	ch, cancel, err := rt.Observe(context.Background(), event.DefaultTenant, h.ID())
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	defer cancel()
	var evs []event.Event
	for ev := range ch {
		evs = append(evs, ev)
	}
	_, _ = h.Wait()
	// The live tail does not replay pre-subscribe events; the durable history is
	// authoritative, so fold it in.
	hist, err := rt.Attach(context.Background(), event.DefaultTenant, h.ID(), 0)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	evs = append(evs, hist...)

	var found bool
	for _, ev := range evs {
		if ev.Kind != event.KindPermissionDecision {
			continue
		}
		var d event.PermissionDecisionPayload
		if json.Unmarshal(ev.Payload, &d) != nil {
			continue
		}
		if d.Verdict == "deny" && d.Layer == permissions.LayerRules && strings.Contains(d.Reason, "bash:touch*") && d.DecidedBy == permissions.DecidedByPolicy {
			found = true
		}
	}
	if !found {
		t.Fatalf("no permission.decision deny/rules naming bash:touch*; events=%d", len(evs))
	}
}

// TestHostedBindingDeniesAlwaysPromptInSmartMode proves, at the composition
// root the hosted loop servers share (buildGate under markPolicyApproval +
// AlwaysApprove), that a smart-mode always_prompt match is denied at the rules
// layer instead of being answered by the policy stand-in, and that the command
// never ran.
func TestHostedBindingDeniesAlwaysPromptInSmartMode(t *testing.T) {
	prev := gateApprovalProvenance
	t.Cleanup(func() { gateApprovalProvenance = prev })
	markPolicyApproval()

	cfg := config.Config{Permissions: config.PermissionsConfig{
		Mode:         "smart",
		AlwaysPrompt: []string{"bash:touch*"},
	}}
	svc, err := sandbox.NewService(sandbox.Config{Contained: false, Handler: sandbox.HandlerHost})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	toolReg := defaultToolRegistry(svc, cfg.Research, nil)
	gate := buildGate(cfg, toolReg, permissions.AlwaysApprove, nil, nil, nil)

	var decisions []permissions.Decision
	ctx := permissions.WithDecisionSink(context.Background(), func(d permissions.Decision) {
		decisions = append(decisions, d)
	})
	dir := t.TempDir()
	t.Chdir(dir)
	sentinel := filepath.Join(dir, "ran")
	res := gate.Execute(ctx, "bash", `{"command":"touch ran"}`)
	if !res.IsError || !res.Denied {
		t.Fatalf("smart-mode always_prompt under the hosted binding must deny; res=%+v decisions=%+v", res, decisions)
	}
	if _, err := os.Stat(sentinel); err == nil {
		t.Fatalf("denied command was executed: %s exists", sentinel)
	}
	var ok bool
	for _, d := range decisions {
		if d.Verdict == "allow" {
			t.Fatalf("no allow decision expected; got %+v", decisions)
		}
		if d.Verdict == "deny" && d.Layer == permissions.LayerRules && d.DecidedBy == permissions.DecidedByPolicy &&
			d.Mode == "smart" && strings.Contains(d.Reason, "bash:touch*") {
			ok = true
		}
	}
	if !ok {
		t.Fatalf("want permission.decision deny/rules/policy in smart mode naming the pattern; got %+v", decisions)
	}
}

// TestLoopServerDeniesAlwaysPromptEndToEnd runs the production loop-server
// runtime in its default mode (no mode key, so smart) against a scripted
// gateway whose model asks for a command matching an operator always_prompt
// rule. With no human bound the call must be denied at the rules layer, visible
// in the durable event stream, and the model must be told why. The harness
// binds no sandbox, so bash could not run here either way; the decision event
// plus TestHostedBindingDeniesAlwaysPromptInSmartMode are the proof of
// non-execution.
func TestLoopServerDeniesAlwaysPromptEndToEnd(t *testing.T) {
	prev := gateApprovalProvenance
	t.Cleanup(func() { gateApprovalProvenance = prev })
	markPolicyApproval()

	args, _ := json.Marshal(map[string]string{"command": "touch ran"})
	callArgs, _ := json.Marshal(string(args))
	toolCall := `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"bash","arguments":` + string(callArgs) + `}}]}}]}`
	final := `{"choices":[{"message":{"role":"assistant","content":"done"}}]}`

	rt, gw, _ := harnessServer(t, harnessOperator+`
permissions:
  always_prompt: ["bash:touch*"]
`)
	gw.script = []string{toolCall, final}

	h, err := rt.StartLoop(context.Background(), runtime.LoopConfig{Task: "go"})
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	ch, cancel, err := rt.Observe(context.Background(), event.DefaultTenant, h.ID())
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	defer cancel()
	for range ch {
	}
	_, _ = h.Wait()
	// Count over the durable history only: the live tail can overlap it, so
	// folding both in would double-count.
	hist, err := rt.Attach(context.Background(), event.DefaultTenant, h.ID(), 0)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}

	var denies []event.PermissionDecisionPayload
	for _, ev := range hist {
		if ev.Kind != event.KindPermissionDecision {
			continue
		}
		var d event.PermissionDecisionPayload
		if json.Unmarshal(ev.Payload, &d) != nil {
			continue
		}
		if d.Tool == "bash" && d.Verdict == "allow" {
			t.Fatalf("bash must not be allowed; got %+v", d)
		}
		if d.Verdict == "deny" {
			denies = append(denies, d)
		}
	}
	if len(denies) != 1 {
		t.Fatalf("want exactly one deny decision; got %+v", denies)
	}
	d := denies[0]
	if d.Mode != "smart" {
		t.Fatalf("hosted default mode is %q, want smart: the spec premise needs re-checking", d.Mode)
	}
	if d.Layer != permissions.LayerRules || d.DecidedBy != permissions.DecidedByPolicy || !strings.Contains(d.Reason, "bash:touch*") {
		t.Fatalf("want deny/rules/policy naming bash:touch*; got %+v", d)
	}

	calls := gw.snapshot()
	if len(calls) < 2 {
		t.Fatalf("want at least 2 gateway calls; got %d", len(calls))
	}
	var told bool
	for _, m := range calls[1].Messages {
		if m.Role == "tool" && strings.Contains(m.Content, "matched ask rule bash:touch*") {
			told = true
		}
	}
	if !told {
		t.Fatalf("model was not told the deny reason; messages=%+v", calls[1].Messages)
	}
}
