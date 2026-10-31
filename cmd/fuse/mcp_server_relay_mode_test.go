package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/hitl"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/tools"
	"github.com/ethanhinson/fuse/internal/tools/sandbox"
)

// TestRelayPermissions pins that only a relayed "off" with a relay socket
// changes the mcp-server's permissions, and that it changes only the mode.
func TestRelayPermissions(t *testing.T) {
	p := config.PermissionsConfig{
		Mode:         "smart",
		AlwaysPrompt: []string{"bash:git push*"},
		Auto:         config.AutoConfig{Ask: []string{"bash:touch*"}},
	}

	got := relayPermissions(p, "/tmp/s.sock", "off")
	if got.Mode != "off" {
		t.Fatalf("relayed off with a socket: Mode = %q, want off", got.Mode)
	}
	got.Mode = "smart"
	if !reflect.DeepEqual(got, p) {
		t.Fatalf("relayed off changed more than the mode: got %+v, want %+v", got, p)
	}

	for _, tc := range []struct{ sock, mode string }{
		{"", "off"},
		{"/tmp/s.sock", "smart"},
		{"/tmp/s.sock", "garbage"},
		{"/tmp/s.sock", ""},
	} {
		if got := relayPermissions(p, tc.sock, tc.mode); !reflect.DeepEqual(got, p) {
			t.Fatalf("relayPermissions(p, %q, %q) = %+v, want p unchanged", tc.sock, tc.mode, got)
		}
	}
}

// TestMCPServerGateApproveAllAcrossRelay builds the mcp-server gate the way
// runMCPServer does, behind a policy-labeled relay socket, and shows that the
// relayed mode is what flips an explicit ask from a rules-layer deny to a
// mode-off allow. The relay approver is never consulted in either case.
func TestMCPServerGateApproveAllAcrossRelay(t *testing.T) {
	work := t.TempDir()
	t.Chdir(work)
	cfg := config.Config{Permissions: config.PermissionsConfig{
		Mode: "auto",
		Auto: config.AutoConfig{Ask: []string{"bash:touch*"}},
	}}
	svc, err := sandbox.NewService(sandbox.Config{Contained: false, Handler: sandbox.HandlerHost})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	toolReg := defaultToolRegistry(svc, cfg.Research, nil)

	// os.MkdirTemp, not t.TempDir: macOS caps a unix socket path at 104 bytes.
	dir, err := os.MkdirTemp("", "hitl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s.sock")
	srv, err := hitl.NewServer(sock, func(context.Context, permissions.ApprovalRequest) (bool, bool, error) {
		t.Error("relay approver must not be asked")
		return false, false, nil
	})
	if err != nil {
		t.Fatalf("hitl.NewServer: %v", err)
	}
	t.Cleanup(srv.Close)

	run := func(t *testing.T, relayMode, marker string) (tools.Result, []permissions.Decision) {
		t.Helper()
		gate := buildMCPServerGate(cfg, toolReg, relayPermissions(cfg.Permissions, sock, relayMode), mcpServerApprover(sock, "policy"))
		var decisions []permissions.Decision
		ctx := permissions.WithDecisionSink(context.Background(), func(d permissions.Decision) {
			decisions = append(decisions, d)
		})
		res := gate.Execute(ctx, "bash", argsJSON(map[string]any{"command": "touch " + marker}))
		return res, decisions
	}

	t.Run("relayed off allows", func(t *testing.T) {
		res, decisions := run(t, "off", "ran")
		if res.Denied {
			t.Fatalf("relayed mode off must allow; res=%+v decisions=%+v", res, decisions)
		}
		if len(decisions) != 1 || decisions[0].Verdict != "allow" || decisions[0].Layer != permissions.LayerModeOff {
			t.Fatalf("decisions = %+v, want exactly one allow/%s", decisions, permissions.LayerModeOff)
		}
		if _, err := os.Stat(filepath.Join(work, "ran")); err != nil {
			t.Fatalf("allowed command did not run: %v", err)
		}
	})

	t.Run("no relayed mode denies", func(t *testing.T) {
		res, decisions := run(t, "", "ran-b")
		if !res.Denied {
			t.Fatalf("without a relayed mode the policy-labeled gate must deny; res=%+v decisions=%+v", res, decisions)
		}
		if len(decisions) != 1 {
			t.Fatalf("decisions = %+v, want exactly one deny", decisions)
		}
		d := decisions[0]
		if d.Verdict != "deny" || d.Layer != permissions.LayerRules || d.DecidedBy != permissions.DecidedByPolicy {
			t.Fatalf("decision = %+v, want deny/%s/%s", d, permissions.LayerRules, permissions.DecidedByPolicy)
		}
		if _, err := os.Stat(filepath.Join(work, "ran-b")); err == nil {
			t.Fatal("denied command was executed: ran-b exists")
		}
	})
}
