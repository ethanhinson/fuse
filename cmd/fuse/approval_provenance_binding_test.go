package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/hitl"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/tools"
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

// TestMCPServerGateProvenanceFollowsSocket builds the headless mcp-server gate
// as runMCPServer does, with the approver hitl.ClientApprovalFunc returns. With
// no socket the approver is the always-approve policy stand-in, so an explicit
// ask is denied at the rules layer with decided_by: policy and never runs. With
// a socket a person in the parent TUI answers, so the same ask prompts and the
// approval records decided_by: human.
func TestMCPServerGateProvenanceFollowsSocket(t *testing.T) {
	// Relative sentinels: path.Match's * does not cross "/".
	work := t.TempDir()
	t.Chdir(work)
	cfg := config.Config{Permissions: config.PermissionsConfig{Mode: "auto", Auto: config.AutoConfig{Ask: []string{"bash:touch*"}}}}
	svc, err := sandbox.NewService(sandbox.Config{Contained: false, Handler: sandbox.HandlerHost})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	toolReg := defaultToolRegistry(svc, cfg.Research, nil)

	t.Run("no socket is policy", func(t *testing.T) {
		gate := buildMCPServerGate(cfg, toolReg, hitl.ClientApprovalFunc(""))
		var decisions []permissions.Decision
		ctx := permissions.WithDecisionSink(context.Background(), func(d permissions.Decision) {
			decisions = append(decisions, d)
		})
		res := gate.Execute(ctx, "bash", argsJSON(map[string]any{"command": "touch ran-a"}))
		if !res.Denied {
			t.Fatalf("socketless mcp-server gate must deny the explicit ask; res=%+v decisions=%+v", res, decisions)
		}
		if _, err := os.Stat(filepath.Join(work, "ran-a")); err == nil {
			t.Fatal("denied command was executed: ran-a exists")
		}
		if len(decisions) != 1 {
			t.Fatalf("decisions = %+v, want exactly one deny", decisions)
		}
		d := decisions[0]
		if d.Verdict != "deny" || d.Layer != permissions.LayerRules || d.DecidedBy != permissions.DecidedByPolicy {
			t.Fatalf("decision = %+v, want deny/%s/%s", d, permissions.LayerRules, permissions.DecidedByPolicy)
		}
	})

	t.Run("socket is human", func(t *testing.T) {
		// os.MkdirTemp, not t.TempDir: a long test name can push a macOS unix
		// socket path past its 104-byte limit.
		dir, err := os.MkdirTemp("", "hitl")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		sockPath := filepath.Join(dir, "s.sock")
		srv, err := hitl.NewServer(sockPath, func(context.Context, permissions.ApprovalRequest) (bool, bool, error) {
			return true, false, nil
		})
		if err != nil {
			t.Fatalf("hitl.NewServer: %v", err)
		}
		t.Cleanup(srv.Close)

		gate := buildMCPServerGate(cfg, toolReg, hitl.ClientApprovalFunc(sockPath))
		var decisions []permissions.Decision
		ctx := permissions.WithDecisionSink(context.Background(), func(d permissions.Decision) {
			decisions = append(decisions, d)
		})
		res := gate.Execute(ctx, "bash", argsJSON(map[string]any{"command": "touch ran-b"}))
		if res.Denied {
			t.Fatalf("socket-bound mcp-server gate must prompt and run; res=%+v decisions=%+v", res, decisions)
		}
		if _, err := os.Stat(filepath.Join(work, "ran-b")); err != nil {
			t.Fatalf("approved command did not run: %v", err)
		}
		if len(decisions) != 2 {
			t.Fatalf("decisions = %+v, want ask then allow", decisions)
		}
		if d := decisions[0]; d.Verdict != "ask" || d.Layer != permissions.LayerRules {
			t.Fatalf("first decision = %+v, want ask/%s", d, permissions.LayerRules)
		}
		if d := decisions[1]; d.Verdict != "allow" || d.Layer != permissions.LayerHuman || d.DecidedBy != permissions.DecidedByHuman {
			t.Fatalf("second decision = %+v, want allow/%s/%s", d, permissions.LayerHuman, permissions.DecidedByHuman)
		}
	})
}

// provenanceProbeTool is a test tool off the safe list that records whether it
// ran, so a gate's denial is observable as "did not run".
type provenanceProbeTool struct{ ran *bool }

func (provenanceProbeTool) Name() string               { return "orch_probe" }
func (provenanceProbeTool) Description() string        { return "provenance probe" }
func (provenanceProbeTool) Parameters() map[string]any { return map[string]any{"type": "object"} }
func (p provenanceProbeTool) Execute(context.Context, string) tools.Result {
	*p.ran = true
	return tools.Result{Output: "ran"}
}

// TestOrchestratorStandInIsPolicy asserts the `fuse orchestrate` orchestrator
// binds a policy approver: its gate, built as buildChildAgent builds it, denies
// a prompting tool call with decided_by: policy and the tool never runs.
func TestOrchestratorStandInIsPolicy(t *testing.T) {
	if orchestrateCLIApprover.DecidedBy != permissions.DecidedByPolicy {
		t.Fatalf("orchestrateCLIApprover.DecidedBy = %q, want %q", orchestrateCLIApprover.DecidedBy, permissions.DecidedByPolicy)
	}

	var ran bool
	orchTools := tools.NewRegistry()
	orchTools.Register(provenanceProbeTool{ran: &ran})
	cfg := config.Config{}

	gate := buildGate(cfg, orchTools, orchestrateCLIApprover, nil, nil, permissions.NewSessionMode(orchestrateCLIMode))
	var decisions []permissions.Decision
	ctx := permissions.WithDecisionSink(context.Background(), func(d permissions.Decision) {
		decisions = append(decisions, d)
	})
	res := gate.Execute(ctx, "orch_probe", "{}")
	if !res.Denied {
		t.Fatalf("orchestrator gate must deny; res=%+v decisions=%+v", res, decisions)
	}
	if ran {
		t.Fatal("denied orchestrator tool ran")
	}
	if len(decisions) == 0 {
		t.Fatal("orchestrator gate recorded no decisions")
	}
	last := decisions[len(decisions)-1]
	if last.Verdict != "deny" || last.Layer != permissions.LayerHuman || last.DecidedBy != permissions.DecidedByPolicy || last.Reason != "tool call denied by user" {
		t.Fatalf("final decision = %+v, want deny/%s/%s with reason %q", last, permissions.LayerHuman, permissions.DecidedByPolicy, "tool call denied by user")
	}

	reg := registryFromConfig(cfg)
	ag, err := orchestrateCLIBuilder(cfg, reg, reg.Default, nil)(discardRenderer{}, orchTools)
	if err != nil {
		t.Fatalf("orchestrateCLIBuilder: %v", err)
	}
	if ag == nil {
		t.Fatal("orchestrateCLIBuilder returned a nil agent")
	}
}
