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
	"github.com/ethanhinson/fuse/internal/tools/sandbox"
)

// unparseableProbe is a command the permission parser refuses (a
// path-qualified argv0) that is harmless if it ever runs: it creates one empty
// file in the test's temp workspace. It is the only command these tests send
// to a real bash.
const unparseableProbe = "/usr/bin/touch ran"

// unparseableDenyPrefixText is the fixed text before the echoed command in
// the reason an unparseable command gets when no approver is bound.
const unparseableDenyPrefixText = "unparseable command denied; no approver bound: "

const unparseableProbeDenyReason = unparseableDenyPrefixText + unparseableProbe

// toolMessage returns the content of the tool-role message in the second
// gateway request (what the model was told after its tool call), or "" when
// there is none.
func toolMessage(gw *recordingGateway) string {
	calls := gw.snapshot()
	if len(calls) < 2 {
		return ""
	}
	for _, m := range calls[1].Messages {
		if m.Role == "tool" {
			return m.Content
		}
	}
	return ""
}

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
// auto.deny rule naming the raw text denies it with the plain rule reason.
// Two more cases prove the echoed command is bounded and redacted on this
// wiring: an over-length command is truncated in the decision event's Reason
// and Command and in what the model is told, and a credentialed URL denied by
// an auto.deny rule has its userinfo replaced with "***@" in all three. The
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
		// probe is the command the model asks for; empty means unparseableProbe.
		probe string
		// check, when set, replaces the exact reason comparison and the
		// modelWasTold check; tool is the tool-role message the model saw.
		check func(t *testing.T, d event.PermissionDecisionPayload, tool string)
	}{
		{desc: "auto mode", yaml: "permissions:\n  mode: auto\n", mode: "auto",
			reason: unparseableProbeDenyReason, decidedBy: permissions.DecidedByPolicy},
		{desc: "smart mode, the default", yaml: "", mode: "smart",
			reason: unparseableProbeDenyReason, decidedBy: permissions.DecidedByPolicy},
		{desc: "auto deny rule on the raw text", yaml: "permissions:\n  mode: auto\n  auto:\n    deny: [\"bash:/usr/bin/*\"]\n",
			mode: "auto", reason: "denied by auto-mode rules layer: " + unparseableProbe},
		{desc: "over-length command is bounded", yaml: "permissions:\n  mode: auto\n", mode: "auto",
			decidedBy: permissions.DecidedByPolicy,
			probe:     "/usr/bin/touch " + strings.Repeat("a", 10000),
			check: func(t *testing.T, d event.PermissionDecisionPayload, tool string) {
				if !strings.HasPrefix(d.Reason, unparseableDenyPrefixText+"/usr/bin/touch") {
					t.Fatalf("reason lost its prefix or command start: %.120q", d.Reason)
				}
				if len(d.Reason) > len(unparseableDenyPrefixText)+203 {
					t.Fatalf("reason is not bounded: %d bytes", len(d.Reason))
				}
				if len(d.Command) > 203 {
					t.Fatalf("command field is not bounded: %d bytes", len(d.Command))
				}
				if tool == "" || len(tool) >= 1024 || !strings.Contains(tool, "\u2026") {
					t.Fatalf("the model was not told a bounded, elided reason (%d bytes): %.120q", len(tool), tool)
				}
			}},
		{desc: "credentialed URL is redacted", yaml: "permissions:\n  mode: auto\n  auto:\n    deny: [\"bash:/usr/bin/curl*\"]\n",
			mode:  "auto",
			probe: "/usr/bin/curl https://u:tok@h/x",
			check: func(t *testing.T, d event.PermissionDecisionPayload, tool string) {
				if want := "denied by auto-mode rules layer: /usr/bin/curl https://***@h/x"; d.Reason != want {
					t.Fatalf("reason = %q, want %q", d.Reason, want)
				}
				if strings.Contains(d.Reason, "tok") || strings.Contains(d.Command, "tok") {
					t.Fatalf("the credential leaked into the decision: %+v", d)
				}
				if !strings.Contains(tool, "***@h/x") || strings.Contains(tool, "tok") {
					t.Fatalf("the model was not told the redacted command: %q", tool)
				}
			}},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			floorWorkspace(t)
			rt, gw, _ := harnessServer(t, harnessOperator+tc.yaml)
			probe := tc.probe
			if probe == "" {
				probe = unparseableProbe
			}
			gw.script = []string{scriptedBashCall(probe), scriptedFinal}
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
				d.DecidedBy != tc.decidedBy {
				t.Fatalf("want deny/rules/%s decided_by %q, got %+v", tc.mode, tc.decidedBy, d)
			}
			if tc.check != nil {
				tc.check(t, d, toolMessage(gw))
				return
			}
			if d.Reason != tc.reason {
				t.Fatalf("want reason %q, got %+v", tc.reason, d)
			}
			if !modelWasTold(gw, tc.reason) {
				t.Fatalf("the model was not told the deny reason %q", tc.reason)
			}
		})
	}
}
