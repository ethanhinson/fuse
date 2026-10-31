package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/tools/sandbox"
)

// unparseableProbe is a command the permission parser refuses (a
// path-qualified argv0) that is harmless if it ever runs: it creates one empty
// file in the test's temp workspace. It is the only command these tests send
// to a real bash.
const unparseableProbe = "/usr/bin/touch ran"

const unparseableProbeDenyReason = "unparseable command denied; no approver bound: " + unparseableProbe

// TestHostedBindingDeniesUnparseableCommand proves, at the composition root
// the loop servers share (buildGate with AlwaysApprove, a policy approver),
// that a command the parser refuses is denied in auto and smart mode and does
// not run (change 0096), while a smart-mode legacy first-token allow naming it
// still runs it (the boundary ADR-0068 pinned).
func TestHostedBindingDeniesUnparseableCommand(t *testing.T) {
	if _, err := os.Stat("/usr/bin/touch"); err != nil {
		t.Fatalf("this test needs /usr/bin/touch on the host: %v", err)
	}
	cases := []struct {
		desc    string
		perms   config.PermissionsConfig
		wantRun bool
	}{
		{"auto mode denies", config.PermissionsConfig{Mode: "auto"}, false},
		{"smart mode denies", config.PermissionsConfig{Mode: "smart"}, false},
		{"smart legacy first-token allow still runs it",
			config.PermissionsConfig{Mode: "smart", AutoApprove: []string{"bash:/usr/bin/touch"}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			_, ws := floorWorkspace(t)
			cfg := config.Config{Permissions: tc.perms}
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
			args, _ := json.Marshal(map[string]string{"command": unparseableProbe})
			res := gate.Execute(ctx, "bash", string(args))
			_, statErr := os.Stat(filepath.Join(ws, "ran"))
			if len(decisions) != 1 {
				t.Fatalf("want exactly one decision, got %+v", decisions)
			}
			d := decisions[0]

			if tc.wantRun {
				if res.IsError || statErr != nil {
					t.Fatalf("the legacy allow must run the command; res=%+v stat=%v", res, statErr)
				}
				if d.Verdict != "allow" || d.Layer != permissions.LayerSmartConfig {
					t.Fatalf("want allow at the smart_config layer, got %+v", d)
				}
				return
			}
			if !res.IsError || !res.Denied {
				t.Fatalf("want a denial, got %+v", res)
			}
			if statErr == nil {
				t.Fatal("the denied command was executed")
			}
			if d.Verdict != "deny" || d.Layer != permissions.LayerRules || d.DecidedBy != permissions.DecidedByPolicy ||
				d.Mode != tc.perms.Mode || d.Reason != unparseableProbeDenyReason {
				t.Fatalf("want deny/rules/policy/%s with the unparseable reason, got %+v", tc.perms.Mode, d)
			}
		})
	}
}

// TestLoopServerDeniesUnparseableCommandEndToEnd runs the production
// loop-server runtime against a scripted gateway whose model asks for a
// command the parser refuses (change 0096). With no human bound it is denied
// at the rules layer in auto mode and in smart mode (the default), and an
// auto.deny rule naming the raw text denies it with the plain rule reason. The
// harness binds no sandbox, so bash could not run here either way; the
// decision event plus TestHostedBindingDeniesUnparseableCommand are the proof
// of non-execution.
func TestLoopServerDeniesUnparseableCommandEndToEnd(t *testing.T) {
	cases := []struct {
		desc      string
		yaml      string
		mode      string
		reason    string
		decidedBy string
	}{
		{"auto mode", "permissions:\n  mode: auto\n", "auto", unparseableProbeDenyReason, permissions.DecidedByPolicy},
		{"smart mode, the default", "", "smart", unparseableProbeDenyReason, permissions.DecidedByPolicy},
		{"auto deny rule on the raw text", "permissions:\n  mode: auto\n  auto:\n    deny: [\"bash:/usr/bin/*\"]\n",
			"auto", "denied by auto-mode rules layer: " + unparseableProbe, ""},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			floorWorkspace(t)
			rt, gw, _ := harnessServer(t, harnessOperator+tc.yaml)
			gw.script = []string{scriptedBashCall(unparseableProbe), scriptedFinal}
			ds := runScriptedLoop(t, rt)

			var bash []event.PermissionDecisionPayload
			for _, d := range ds {
				if d.Tool != "bash" {
					continue
				}
				if d.Verdict == "allow" {
					t.Fatalf("the refused command must not be allowed; got %+v", d)
				}
				if d.Verdict == "ask" && d.Layer == permissions.LayerParse {
					t.Fatalf("no parse-layer ask expected with no human bound; got %+v", d)
				}
				bash = append(bash, d)
			}
			if len(bash) != 1 {
				t.Fatalf("want exactly one bash decision, got %+v", bash)
			}
			d := bash[0]
			if d.Verdict != "deny" || d.Layer != permissions.LayerRules || d.Mode != tc.mode ||
				d.DecidedBy != tc.decidedBy || d.Reason != tc.reason {
				t.Fatalf("want deny/rules/%s decided_by %q reason %q, got %+v", tc.mode, tc.decidedBy, tc.reason, d)
			}
			if !modelWasTold(gw, tc.reason) {
				t.Fatalf("the model was not told the deny reason %q", tc.reason)
			}
		})
	}
}
