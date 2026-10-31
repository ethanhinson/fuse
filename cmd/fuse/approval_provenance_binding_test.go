package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/tools/sandbox"
)

// TestHumanAttendedRunWithAlwaysApproveChild builds, in one process, a root gate
// bound to a human approver and a child gate bound to the always-approve policy
// stand-in. Provenance belongs to each gate's approver, so the root prompts its
// human and records decided_by: human, while the child denies the same explicit
// ask at the rules layer with decided_by: policy and never runs the command. No
// process-wide state is touched.
func TestHumanAttendedRunWithAlwaysApproveChild(t *testing.T) {
	cases := []struct {
		name  string
		perms config.PermissionsConfig
	}{
		{"auto", config.PermissionsConfig{Mode: "auto", Auto: config.AutoConfig{Ask: []string{"bash:touch*"}}}},
		{"smart", config.PermissionsConfig{Mode: "smart", AlwaysPrompt: []string{"bash:touch*"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Relative sentinels: path.Match's * does not cross "/", so the
			// pattern bash:touch* only matches a slash-free argv.
			dir := t.TempDir()
			t.Chdir(dir)
			cfg := config.Config{Permissions: tc.perms}
			svc, err := sandbox.NewService(sandbox.Config{Contained: false, Handler: sandbox.HandlerHost})
			if err != nil {
				t.Fatalf("NewService: %v", err)
			}
			toolReg := defaultToolRegistry(svc, cfg.Research, nil)

			calls := 0
			recorder := func(context.Context, permissions.ApprovalRequest) (bool, bool, error) {
				calls++
				return true, false, nil
			}
			root := buildGate(cfg, toolReg, permissions.Human(recorder), nil, nil, nil)
			child := buildGate(cfg, toolReg, permissions.AlwaysApprove, nil, nil, nil)

			var rootDecisions, childDecisions []permissions.Decision
			ctxRoot := permissions.WithDecisionSink(context.Background(), func(d permissions.Decision) {
				rootDecisions = append(rootDecisions, d)
			})
			ctxChild := permissions.WithDecisionSink(context.Background(), func(d permissions.Decision) {
				childDecisions = append(childDecisions, d)
			})

			res := root.Execute(ctxRoot, "bash", argsJSON(map[string]any{"command": "touch ran-root"}))
			if res.Denied {
				t.Fatalf("human-bound root must prompt and run; res=%+v decisions=%+v", res, rootDecisions)
			}
			if calls != 1 {
				t.Fatalf("root approver calls = %d, want 1", calls)
			}
			if _, err := os.Stat(filepath.Join(dir, "ran-root")); err != nil {
				t.Fatalf("approved root command did not run: %v", err)
			}
			if len(rootDecisions) == 0 {
				t.Fatal("root recorded no decisions")
			}
			last := rootDecisions[len(rootDecisions)-1]
			if last.Verdict != "allow" || last.Layer != permissions.LayerHuman || last.DecidedBy != permissions.DecidedByHuman {
				t.Fatalf("root final decision = %+v, want allow/%s/%s", last, permissions.LayerHuman, permissions.DecidedByHuman)
			}

			res = child.Execute(ctxChild, "bash", argsJSON(map[string]any{"command": "touch ran-child"}))
			if !res.Denied {
				t.Fatalf("policy-bound child must deny the explicit ask; res=%+v decisions=%+v", res, childDecisions)
			}
			if calls != 1 {
				t.Fatalf("child reached the root's human approver: calls = %d, want 1", calls)
			}
			if _, err := os.Stat(filepath.Join(dir, "ran-child")); err == nil {
				t.Fatal("denied child command was executed: ran-child exists")
			}
			if len(childDecisions) != 1 {
				t.Fatalf("child decisions = %+v, want exactly one deny", childDecisions)
			}
			d := childDecisions[0]
			if d.Verdict != "deny" || d.Layer != permissions.LayerRules || d.DecidedBy != permissions.DecidedByPolicy || !strings.Contains(d.Reason, "bash:touch*") {
				t.Fatalf("child decision = %+v, want deny/%s/%s naming bash:touch*", d, permissions.LayerRules, permissions.DecidedByPolicy)
			}
			for _, cd := range childDecisions {
				if cd.Verdict == "allow" {
					t.Fatalf("child recorded an allow decision: %+v", cd)
				}
			}
		})
	}
}
