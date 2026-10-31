package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/tools/sandbox"
)

// TestMCPServerGateBindsWorkspaceRoot builds the headless mcp-server gate
// exactly as runMCPServer does (buildMCPServerGate, policy stand-in) and runs
// `rm -rf ..` from inside a workspace (change 0093). With the workspace root
// bound, the floor recognises the workspace ancestor and denies it at the
// rules layer in auto and smart mode alike; the workspace survives.
func TestMCPServerGateBindsWorkspaceRoot(t *testing.T) {
	for _, mode := range []string{"auto", "smart"} {
		t.Run(mode, func(t *testing.T) {
			prev := gateApprovalProvenance
			t.Cleanup(func() { gateApprovalProvenance = prev })
			markPolicyApproval()

			base, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			ws := filepath.Join(base, "ws")
			if err := os.Mkdir(ws, 0o755); err != nil {
				t.Fatal(err)
			}
			t.Chdir(ws)

			cfg := config.Config{Permissions: config.PermissionsConfig{Mode: mode}}
			svc, err := sandbox.NewService(sandbox.Config{Contained: false, Handler: sandbox.HandlerHost})
			if err != nil {
				t.Fatalf("NewService: %v", err)
			}
			toolReg := defaultToolRegistry(svc, cfg.Research, nil)
			gate := buildMCPServerGate(cfg, toolReg, permissions.AlwaysApprove)

			var decisions []permissions.Decision
			ctx := permissions.WithDecisionSink(context.Background(), func(d permissions.Decision) {
				decisions = append(decisions, d)
			})
			res := gate.Execute(ctx, "bash", `{"command":"rm -rf .."}`)
			if !res.Denied {
				t.Fatalf("rm -rf .. under the mcp-server gate must deny; res=%+v decisions=%+v", res, decisions)
			}
			if _, err := os.Stat(ws); err != nil {
				t.Fatalf("workspace ancestor deletion ran: %v", err)
			}
			var ok bool
			for _, d := range decisions {
				if d.Verdict == "allow" {
					t.Fatalf("no allow decision expected; got %+v", decisions)
				}
				if d.Verdict == "deny" && d.Layer == permissions.LayerRules {
					ok = true
				}
			}
			if !ok {
				t.Fatalf("want a deny at the rules layer; got %+v", decisions)
			}
		})
	}
}
