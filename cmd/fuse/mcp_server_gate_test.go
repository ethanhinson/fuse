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

// mcpWriteLayers runs a write_file of <dir>/x.txt through the mcp-server gate
// in auto mode with a human-bound approver that denies, and returns the
// decision layers seen.
func mcpWriteLayers(t *testing.T, target string) []permissions.Decision {
	t.Helper()
	cfg := config.Config{Permissions: config.PermissionsConfig{Mode: "auto"}}
	svc, err := sandbox.NewService(sandbox.Config{Contained: false, Handler: sandbox.HandlerHost})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	toolReg := defaultToolRegistry(svc, cfg.Research, nil)
	deny := func(context.Context, permissions.ApprovalRequest) (bool, bool, error) { return false, false, nil }
	gate := buildMCPServerGate(cfg, toolReg, deny)
	var decisions []permissions.Decision
	ctx := permissions.WithDecisionSink(context.Background(), func(d permissions.Decision) {
		decisions = append(decisions, d)
	})
	gate.Execute(ctx, "write_file", `{"path":"`+target+`","content":"x"}`)
	return decisions
}

func hasHeuristicAllow(ds []permissions.Decision) bool {
	for _, d := range ds {
		if d.Verdict == "allow" && d.Layer != permissions.LayerHuman {
			return true
		}
	}
	return false
}

// A server spawned with cwd == $HOME binds no workspace root, so a write under
// home is not heuristically approved.
func TestMCPServerGateHomeCwdBindsNoRoot(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Chdir(home)
	if got := mcpServerWorkspaceRoot(); got != "" {
		t.Fatalf("home cwd must bind no root, got %q", got)
	}
	ds := mcpWriteLayers(t, filepath.Join(home, "Documents", "x.txt"))
	if hasHeuristicAllow(ds) {
		t.Fatalf("write under home cwd must not be heuristically approved: %+v", ds)
	}
}

// A normal project cwd still scopes as before.
func TestMCPServerGateProjectCwdScopes(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	proj := filepath.Join(home, "proj")
	if err := os.Mkdir(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(proj)
	if got := mcpServerWorkspaceRoot(); got != proj {
		t.Fatalf("project cwd root = %q, want %q", got, proj)
	}
	ds := mcpWriteLayers(t, filepath.Join(proj, "x.txt"))
	if !hasHeuristicAllow(ds) {
		t.Fatalf("in-root write should be heuristically approved: %+v", ds)
	}
}
