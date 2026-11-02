package main

import (
	"context"
	"io"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/permissions"
)

// TestPipelineRoleStepAtGateway (change 0115): an authored pipeline run from a
// real binding's root builds each role step exactly like a spawn_agent role
// child. The reviewer step gets the reviewer brief after the persona, the
// role's model pin and the role's tools plus the blackboard tools. The writer
// step gets its brief and the plain step's tools. The plain step gets no
// brief. An unknown role is refused before any step spawns.
func TestPipelineRoleStepAtGateway(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	gw := &routedGateway{route: func(string, int, gatewayCall) string { return textReply("done") }}
	srv := httptest.NewServer(gw)
	t.Cleanup(srv.Close)
	cfg, err := config.FromYAML([]byte(roleOperatorYAML), "test")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Gateway.URL, cfg.Gateway.Key = srv.URL, "tkn"
	reg := registryFromConfig(cfg)
	toolReg := defaultToolRegistry(nil, cfg.Research, nil)
	tree := agent.NewAgentTreeWithConcurrency(reg.Default, reg.Default, cfg.Agents.MaxConcurrent)
	deps, closeMCP := buildOneShotRuntimeDeps(cfg, reg, reg.Default, toolReg, tree, io.Discard, false, nil,
		permissions.AlwaysApprove, "block", false, nil, nil)
	t.Cleanup(closeMCP)
	if _, _, _, err := deps.BuildAgent(nil, tree, reg.Default, toolReg); err != nil {
		t.Fatalf("BuildAgent: %v", err)
	}

	const def = "name: rp\nsteps:\n" +
		"  - name: rev\n    role: reviewer\n    prompt: REV-PIPE-TASK\n" +
		"  - name: wri\n    role: writer\n    prompt: WRI-PIPE-TASK\n" +
		"  - name: pln\n    prompt: PLN-PIPE-TASK\n"
	res := toolReg.Execute(context.Background(), "pipeline_run", argsJSON(map[string]any{"definition": def}))
	if res.IsError || res.Output != "pipeline rp: completed" {
		t.Fatalf("pipeline_run = %+v, want completed", res)
	}

	house := personaPromptFor(t, roleOperatorYAML, "house")
	fast := personaPromptFor(t, roleOperatorYAML, "fast")
	system := func(marker string) (gatewayCall, string) {
		t.Helper()
		c := turnRequest(t, gw, marker, 0)
		if len(c.Messages) == 0 || c.Messages[0].Role != "system" {
			t.Fatalf("%s: first message is not the system prompt", marker)
		}
		return c, c.Messages[0].Content
	}

	rev, revSys := system("REV-PIPE-TASK")
	assertInOrder(t, revSys, house, "REVIEWER-BRIEF")
	if rev.Model != "cloud/house" {
		t.Errorf("reviewer step model = %q, want the role's pin cloud/house", rev.Model)
	}
	reviewerTools := []string{"blackboard_delete", "blackboard_keys", "blackboard_read", "blackboard_wait", "blackboard_write", "grep", "read_file"}
	if got := sortedToolNames(rev); !reflect.DeepEqual(got, reviewerTools) {
		t.Errorf("reviewer step tools = %v, want %v", got, reviewerTools)
	}

	wri, wriSys := system("WRI-PIPE-TASK")
	assertInOrder(t, wriSys, fast, "WRITER-BRIEF")
	if wri.Model != "cloud/fast" {
		t.Errorf("writer step model = %q, want the parent's cloud/fast", wri.Model)
	}
	pln, plnSys := system("PLN-PIPE-TASK")
	if got, want := sortedToolNames(wri), sortedToolNames(pln); !reflect.DeepEqual(got, want) {
		t.Errorf("writer step tools = %v, want the plain step's %v", got, want)
	}
	if strings.Contains(plnSys, "REVIEWER-BRIEF") || strings.Contains(plnSys, "WRITER-BRIEF") {
		t.Errorf("plain step carries a role brief:\n%s", plnSys)
	}
	for label, want := range map[string]string{"rev": "reviewer", "wri": "writer", "pln": ""} {
		if got := nodeLabelled(t, tree, label).Role; got != want {
			t.Errorf("step %q Role = %q, want %q", label, got, want)
		}
	}

	before := len(tree.SnapshotAll())
	bad := toolReg.Execute(context.Background(), "pipeline_run",
		argsJSON(map[string]any{"definition": "name: bad\nsteps:\n  - name: x\n    role: nope\n    prompt: X-PIPE-TASK\n"}))
	if !bad.IsError || !strings.Contains(bad.Output, `unknown role "nope" (available: reviewer, writer)`) {
		t.Fatalf("unknown-role pipeline_run = %+v, want the unknown-role refusal", bad)
	}
	if n := len(tree.SnapshotAll()); n != before {
		t.Fatalf("refused pipeline created %d nodes, want 0", n-before)
	}

	var desc string
	for _, tl := range toolReg.Tools() {
		if tl.Name() == "pipeline_run" {
			desc = tl.Description()
		}
	}
	if !strings.Contains(desc, "`role`") || !strings.Contains(desc, "\n- reviewer — ") {
		t.Errorf("root pipeline_run description does not mention roles: %q", desc)
	}
}
