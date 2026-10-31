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

// floorWorkspace chdirs into a fresh, symlink-resolved <tmp>/ws and returns
// (base, ws).
func floorWorkspace(t *testing.T) (string, string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ws := filepath.Join(base, "ws")
	if err := os.Mkdir(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(ws)
	return base, ws
}

// scriptedBashCall renders one chat-completions reply asking for a bash call.
// Arguments are marshalled from plain values exactly once per layer, never
// hand-escaped (see the scripted-gateway double-escaping finding).
func scriptedBashCall(command string) string {
	args, _ := json.Marshal(map[string]string{"command": command})
	callArgs, _ := json.Marshal(string(args))
	return `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"bash","arguments":` + string(callArgs) + `}}]}}]}`
}

const scriptedFinal = `{"choices":[{"message":{"role":"assistant","content":"done"}}]}`

// runScriptedLoop starts one loop on rt, drains Observe, waits, and returns
// the permission.decision payloads from the durable history only (the live
// tail can overlap it, so folding both would double-count).
func runScriptedLoop(t *testing.T, rt runtime.Runtime) []event.PermissionDecisionPayload {
	t.Helper()
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
	hist, err := rt.Attach(context.Background(), event.DefaultTenant, h.ID(), 0)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	var out []event.PermissionDecisionPayload
	for _, ev := range hist {
		if ev.Kind != event.KindPermissionDecision {
			continue
		}
		var d event.PermissionDecisionPayload
		if json.Unmarshal(ev.Payload, &d) == nil {
			out = append(out, d)
		}
	}
	return out
}

// modelWasTold reports whether the second gateway request carried a tool-role
// message containing want.
func modelWasTold(gw *recordingGateway, want string) bool {
	calls := gw.snapshot()
	if len(calls) < 2 {
		return false
	}
	for _, m := range calls[1].Messages {
		if m.Role == "tool" && strings.Contains(m.Content, want) {
			return true
		}
	}
	return false
}

// TestHostedBindingDeniesCatastrophicInSmartMode proves, at the composition
// root the loop servers share (buildGate under markPolicyApproval +
// AlwaysApprove), that a smart-mode legacy allow cannot approve a workspace
// ancestor deletion (change 0093): it is denied at the rules layer and the
// sentinel survives.
func TestHostedBindingDeniesCatastrophicInSmartMode(t *testing.T) {
	prev := gateApprovalProvenance
	t.Cleanup(func() { gateApprovalProvenance = prev })
	markPolicyApproval()

	_, ws := floorWorkspace(t)
	keep := filepath.Join(ws, "keep")
	if err := os.Mkdir(keep, 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{Permissions: config.PermissionsConfig{
		Mode:        "smart",
		AutoApprove: []string{"bash:rm"},
	}}
	// buildGate binds the workspace root through autoModeOptions, which only
	// applies when a gateway is configured, as it is in every loop-server
	// binding. Smart mode never consults the classifier, so this is never dialled.
	cfg.Gateway.URL = "http://127.0.0.1:1"
	svc, err := sandbox.NewService(sandbox.Config{Contained: false, Handler: sandbox.HandlerHost})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	toolReg := defaultToolRegistry(svc, cfg.Research, nil)
	gate := buildGate(cfg, toolReg, permissions.AlwaysApprove, registryFromConfig(cfg), nil, nil)

	var decisions []permissions.Decision
	ctx := permissions.WithDecisionSink(context.Background(), func(d permissions.Decision) {
		decisions = append(decisions, d)
	})
	res := gate.Execute(ctx, "bash", `{"command":"rm -rf .."}`)
	if !res.IsError || !res.Denied {
		t.Fatalf("rm -rf .. under the hosted smart binding must deny; res=%+v decisions=%+v", res, decisions)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("denied command was executed: %v", err)
	}
	var ok bool
	for _, d := range decisions {
		if d.Verdict == "allow" {
			t.Fatalf("no allow decision expected; got %+v", decisions)
		}
		if d.Verdict == "deny" && d.Layer == permissions.LayerRules && d.DecidedBy == permissions.DecidedByPolicy &&
			d.Mode == "smart" && strings.HasPrefix(d.Reason, "catastrophic command denied by rules floor:") {
			ok = true
		}
	}
	if !ok {
		t.Fatalf("want deny/rules/policy/smart with the floor reason; got %+v", decisions)
	}
}

// TestLoopServerDeniesWrapperRuleEndToEnd runs the production loop-server
// runtime against a scripted gateway whose model asks for a wrapped command
// (change 0093). A protective rule naming the wrapper must fire: in auto mode
// an auto.deny rule denies at the rules layer, and in smart mode (the default)
// an always_prompt rule with argument text, reachable only through the raw
// pre-peel subject, is denied by policy. The harness binds no sandbox, so bash
// could not run here either way; the decision events are the proof.
func TestLoopServerDeniesWrapperRuleEndToEnd(t *testing.T) {
	t.Run("auto deny", func(t *testing.T) {
		prev := gateApprovalProvenance
		t.Cleanup(func() { gateApprovalProvenance = prev })
		markPolicyApproval()
		floorWorkspace(t)

		rt, gw, _ := harnessServer(t, harnessOperator+`
permissions:
  mode: auto
  auto:
    deny: ["bash:timeout*"]
`)
		gw.script = []string{scriptedBashCall("timeout 5 touch ran"), scriptedFinal}
		ds := runScriptedLoop(t, rt)

		var denies []event.PermissionDecisionPayload
		for _, d := range ds {
			if d.Tool == "bash" && d.Verdict == "allow" {
				t.Fatalf("the wrapped call must not be allowed; got %+v", d)
			}
			if d.Verdict == "deny" {
				denies = append(denies, d)
			}
		}
		if len(denies) != 1 {
			t.Fatalf("want exactly one deny; got %+v", denies)
		}
		d := denies[0]
		if d.Layer != permissions.LayerRules || d.Mode != "auto" ||
			!strings.HasPrefix(d.Reason, "denied by auto-mode rules layer:") || !strings.Contains(d.Reason, "timeout 5 touch ran") {
			t.Fatalf("want deny/rules/auto naming the command; got %+v", d)
		}
		if !modelWasTold(gw, d.Reason) {
			t.Fatalf("model was not told the deny reason %q", d.Reason)
		}
	})

	t.Run("smart always_prompt", func(t *testing.T) {
		prev := gateApprovalProvenance
		t.Cleanup(func() { gateApprovalProvenance = prev })
		markPolicyApproval()
		floorWorkspace(t)

		// "bash:timeout *" (with a space) misses smart mode's legacy
		// "bash:timeout" head, so only the raw pre-peel subject reaches it.
		rt, gw, _ := harnessServer(t, harnessOperator+`
permissions:
  always_prompt: ["bash:timeout *"]
`)
		gw.script = []string{scriptedBashCall("timeout 5 touch ran"), scriptedFinal}
		ds := runScriptedLoop(t, rt)

		var denies []event.PermissionDecisionPayload
		for _, d := range ds {
			if d.Tool == "bash" && d.Verdict == "allow" {
				t.Fatalf("the wrapped call must not be allowed; got %+v", d)
			}
			if d.Verdict == "deny" {
				denies = append(denies, d)
			}
		}
		if len(denies) != 1 {
			t.Fatalf("want exactly one deny; got %+v", denies)
		}
		d := denies[0]
		if d.Mode != "smart" || d.Layer != permissions.LayerRules || d.DecidedBy != permissions.DecidedByPolicy ||
			!strings.Contains(d.Reason, "bash:timeout *") {
			t.Fatalf("want deny/rules/policy/smart naming bash:timeout *; got %+v", d)
		}
		if !modelWasTold(gw, "matched ask rule bash:timeout *") {
			t.Fatal("model was not told the deny reason")
		}
	})
}

// TestLoopServerDeniesCatastrophicInSmartModeEndToEnd runs the production
// loop-server runtime in its default smart mode with a legacy allow naming rm,
// against a scripted model that asks for a workspace ancestor deletion (change
// 0093). With no human bound it must be denied at the rules layer by the
// floor. The harness binds no sandbox; the decision event plus
// TestHostedBindingDeniesCatastrophicInSmartMode are the proof of
// non-execution.
func TestLoopServerDeniesCatastrophicInSmartModeEndToEnd(t *testing.T) {
	prev := gateApprovalProvenance
	t.Cleanup(func() { gateApprovalProvenance = prev })
	markPolicyApproval()
	floorWorkspace(t)

	rt, gw, _ := harnessServer(t, harnessOperator+`
permissions:
  auto_approve: ["bash:rm"]
`)
	gw.script = []string{scriptedBashCall("rm -rf .."), scriptedFinal}
	ds := runScriptedLoop(t, rt)

	var denies []event.PermissionDecisionPayload
	for _, d := range ds {
		if d.Tool == "bash" && d.Verdict == "allow" {
			t.Fatalf("rm -rf .. must not be allowed; got %+v", d)
		}
		if d.Verdict == "deny" {
			denies = append(denies, d)
		}
	}
	if len(denies) != 1 {
		t.Fatalf("want exactly one deny; got %+v", denies)
	}
	d := denies[0]
	if d.Mode != "smart" || d.Layer != permissions.LayerRules || d.DecidedBy != permissions.DecidedByPolicy ||
		!strings.HasPrefix(d.Reason, "catastrophic command denied by rules floor:") {
		t.Fatalf("want deny/rules/policy/smart with the floor reason; got %+v", d)
	}
	if !modelWasTold(gw, "catastrophic command denied by rules floor:") {
		t.Fatal("model was not told the deny reason")
	}
}
