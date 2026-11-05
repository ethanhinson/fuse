package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/tools/sandbox"
)

// TestHostedBindingCompoundCdPwd_AutoApproves is the composition-root
// assertion for change 0142: the loop-server binding (buildGate with
// AlwaysApprove, the real gate construction) auto-approves a
// `cd "$(pwd)" && git status` compound at the heuristic layer rather than
// asking. The gate is built exactly as the loop servers build it (buildGate),
// with a workspace root from floorWorkspace and no classifier — the compound
// is provably safe at the deterministic layer, so no classifier is needed.
//
// Before 0142 the opaque `cd "$(pwd)"` arg hit the hasOpaqueArg ask and the
// whole compound was routed to the human at LayerHeuristic; after 0142 `cd` is
// non-mutating and the compound auto-approves.
func TestHostedBindingCompoundCdPwd_AutoApproves(t *testing.T) {
	_, ws := floorWorkspace(t)
	cfg := config.Config{Permissions: config.PermissionsConfig{Mode: "auto"}}
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
	// `cd "$(pwd)" && git status` — pwd outputs ws (the workspace root), so
	// the cd target is provably in-root, and git status is read-only-safe.
	// The gate auto-approves it (the decision proves that); the bash tool
	// then runs it and `git status` may error (no git repo here), so the test
	// asserts on the DECISION, not on res.IsError.
	args, _ := json.Marshal(map[string]string{"command": `cd "$(pwd)" && git status`})
	res := gate.Execute(ctx, "bash", string(args))
	_ = res
	if len(decisions) != 1 {
		t.Fatalf("want exactly one decision, got %+v", decisions)
	}
	d := decisions[0]
	if d.Verdict != "allow" {
		t.Fatalf("want allow, got %q (layer %s, reason %q)", d.Verdict, d.Layer, d.Reason)
	}
	// The compound is provably safe at the heuristic layer (cd is
	// non-mutating, git status is read-only) — not the safelist (cd is not
	// read-only) and not the classifier (no gateway is wired).
	if d.Layer != permissions.LayerHeuristic {
		t.Fatalf("want the heuristic layer (cd is non-mutating), got %q", d.Layer)
	}
	// Verify the cwd did not actually change (cd in a compound is scoped to
	// the bash execution, not the test process).
	if _, err := os.Stat(filepath.Join(ws, "should-not-exist")); err == nil {
		t.Fatal("the compound should not have created files in the workspace")
	}
}

// TestHostedBindingCompoundCdPwd_DenyForcedByPolicy pins that a policy
// approver (AlwaysApprove) with a catastrophic compound still DENIES at the
// rules layer — the compound decomposition does not let a safe segment mask
// a catastrophic one at the composition root.
func TestHostedBindingCompoundCdPwd_DenyForcedByPolicy(t *testing.T) {
	_, _ = floorWorkspace(t)
	cfg := config.Config{Permissions: config.PermissionsConfig{Mode: "auto"}}
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
	args, _ := json.Marshal(map[string]string{"command": `cd "$(pwd)" && rm -rf ~`})
	res := gate.Execute(ctx, "bash", string(args))
	if !res.IsError {
		t.Fatalf("catastrophic compound must be denied, got success: %+v", res)
	}
	if len(decisions) != 1 {
		t.Fatalf("want exactly one decision, got %+v", decisions)
	}
	d := decisions[0]
	if d.Verdict != "deny" || d.Layer != permissions.LayerRules {
		t.Fatalf("want deny at the rules layer, got %+v", d)
	}
}
