package main

import (
	"context"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/permissions"
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
		Auto: config.AutoConfig{Ask: []string{"bash:cat*"}},
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
	res := gate.Execute(ctx, "bash", `{"command":"cat missing.txt"}`)
	if !res.IsError || !res.Denied {
		t.Fatalf("explicit ask under the hosted binding must deny; res=%+v decisions=%+v", res, decisions)
	}
	if strings.Contains(res.Output, "No such file") {
		t.Fatalf("denied command was executed: %q", res.Output)
	}
	var ok bool
	for _, d := range decisions {
		if d.Verdict == "deny" && d.Layer == permissions.LayerRules && strings.Contains(d.Reason, "bash:cat*") {
			ok = true
		}
	}
	if !ok {
		t.Fatalf("want permission.decision deny/rules naming the pattern; got %+v", decisions)
	}
}
