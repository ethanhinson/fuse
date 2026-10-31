package permissions

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
)

// askTouchCfg is an auto-mode config whose only rule is an explicit ask on
// `touch`, so the approver's provenance alone decides the outcome.
func askTouchCfg() config.PermissionsConfig {
	return autoCfg(config.AutoConfig{Ask: []string{"bash:touch*"}}, nil, nil)
}

// canonTempDir returns a symlink-resolved temp dir and chdirs into it for the
// test, so workspace-relative path checks see the same root.
func canonTempDir(t *testing.T) string {
	t.Helper()
	canon, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(chdir(t, canon))
	return canon
}

// TestNew_ProvenanceIsTheApprovers proves a gate's provenance comes from the
// approver it was built with and nothing else: AlwaysApprove alone (no option)
// is policy, and a Human-wrapped func prompts and records human.
func TestNew_ProvenanceIsTheApprovers(t *testing.T) {
	canon := canonTempDir(t)
	cfg := askTouchCfg()

	t.Run("AlwaysApprove is policy", func(t *testing.T) {
		ctx, rec := recordDecisions()
		g := New(cfg, newTestRegistry("bash"), AlwaysApprove, WithWorkspaceRoot(canon))
		g.Execute(ctx, "bash", bashArgs("touch x"))
		if len(*rec) != 1 {
			t.Fatalf("want exactly one decision, got %+v", *rec)
		}
		d := (*rec)[0]
		if d.Verdict != "deny" || d.Layer != LayerRules || d.DecidedBy != DecidedByPolicy {
			t.Fatalf("want rules deny decided by policy, got %+v", d)
		}
		if want := "matched ask rule bash:touch*; no approver bound"; d.Reason != want {
			t.Fatalf("reason = %q, want %q", d.Reason, want)
		}
	})

	t.Run("Human prompts", func(t *testing.T) {
		ctx, rec := recordDecisions()
		approve, called := newApproveRecorder(true)
		g := New(cfg, newTestRegistry("bash"), Human(approve), WithWorkspaceRoot(canon))
		g.Execute(ctx, "bash", bashArgs("touch x"))
		if !*called {
			t.Fatal("a human approver must be prompted for an explicit ask")
		}
		if len(*rec) != 2 {
			t.Fatalf("want ask then allow, got %+v", *rec)
		}
		if a := (*rec)[0]; a.Verdict != "ask" || a.Layer != LayerRules {
			t.Fatalf("first decision = %+v, want rules ask", a)
		}
		if a := (*rec)[1]; a.Verdict != "allow" || a.Layer != LayerHuman || a.DecidedBy != DecidedByHuman {
			t.Fatalf("second decision = %+v, want human allow", a)
		}
	})
}

// mustPanic runs fn and fails unless it panics; it returns the panic message.
func mustPanic(t *testing.T, name string, fn func()) (msg string) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Errorf("%s: want a panic, got none", name)
			return
		}
		msg = fmt.Sprint(r)
	}()
	fn()
	return ""
}

// TestNew_RejectsUnlabelledApprover proves New fails closed on an approver with
// no function or with a provenance that is neither human nor policy, and that
// the panic tells the caller which constructor to use.
func TestNew_RejectsUnlabelledApprover(t *testing.T) {
	cfg := askTouchCfg()
	reg := newTestRegistry("bash")
	cases := map[string]Approver{
		"no provenance":      {Answer: AlwaysApprove.Answer},
		"Human(nil)":         Human(nil),
		"Policy(nil)":        Policy(nil),
		"unknown provenance": {Answer: AlwaysApprove.Answer, DecidedBy: "robot"},
	}
	for name, a := range cases {
		msg := mustPanic(t, name, func() { New(cfg, reg, a) })
		if !strings.Contains(msg, "permissions.Human") || !strings.Contains(msg, "permissions.Policy") {
			t.Errorf("%s: panic %q must name permissions.Human and permissions.Policy", name, msg)
		}
	}
}

// TestCloneForChild_KeepsApproverProvenance proves a child gate keeps its
// parent's approver: a human parent's child prompts with the [label] prefix and
// records human; a policy parent's child denies the explicit ask as policy.
func TestCloneForChild_KeepsApproverProvenance(t *testing.T) {
	canon := canonTempDir(t)
	cfg := askTouchCfg()

	t.Run("human parent", func(t *testing.T) {
		var previews []string
		rec := func(_ context.Context, req ApprovalRequest) (bool, bool, error) {
			previews = append(previews, req.Preview)
			return true, false, nil
		}
		parent := New(cfg, newTestRegistry("bash"), Human(rec), WithWorkspaceRoot(canon))
		child := parent.CloneForChild("w")
		ctx, decisions := recordDecisions()
		child.Execute(ctx, "bash", bashArgs("touch x"))
		if len(previews) != 1 || !strings.HasPrefix(previews[0], "[w] ") {
			t.Fatalf("child prompt previews = %q, want one starting with [w]", previews)
		}
		if len(*decisions) == 0 {
			t.Fatal("no decisions recorded")
		}
		last := (*decisions)[len(*decisions)-1]
		if last.Verdict != "allow" || last.Layer != LayerHuman || last.DecidedBy != DecidedByHuman {
			t.Fatalf("final child decision = %+v, want human allow", last)
		}
	})

	t.Run("policy parent", func(t *testing.T) {
		parent := New(cfg, newTestRegistry("bash"), AlwaysApprove, WithWorkspaceRoot(canon))
		child := parent.CloneForChild("w")
		ctx, decisions := recordDecisions()
		child.Execute(ctx, "bash", bashArgs("touch x"))
		if len(*decisions) != 1 {
			t.Fatalf("want exactly one decision, got %+v", *decisions)
		}
		if d := (*decisions)[0]; d.Verdict != "deny" || d.Layer != LayerRules || d.DecidedBy != DecidedByPolicy {
			t.Fatalf("child decision = %+v, want rules deny decided by policy", d)
		}
	})
}

// TestPrefixApproval_KeepsProvenance proves PrefixApproval keeps who answers
// while prefixing the prompt preview with the child label.
func TestPrefixApproval_KeepsProvenance(t *testing.T) {
	if got := PrefixApproval("w", AlwaysApprove).DecidedBy; got != DecidedByPolicy {
		t.Errorf("PrefixApproval(AlwaysApprove).DecidedBy = %q, want policy", got)
	}
	var seen string
	fn := func(_ context.Context, req ApprovalRequest) (bool, bool, error) {
		seen = req.Preview
		return true, false, nil
	}
	prefixed := PrefixApproval("w", Human(fn))
	if prefixed.DecidedBy != DecidedByHuman {
		t.Errorf("PrefixApproval(Human).DecidedBy = %q, want human", prefixed.DecidedBy)
	}
	if _, _, err := prefixed.Answer(context.Background(), ApprovalRequest{Preview: "p"}); err != nil {
		t.Fatal(err)
	}
	if seen != "[w] p" {
		t.Errorf("prefixed preview = %q, want %q", seen, "[w] p")
	}
}
