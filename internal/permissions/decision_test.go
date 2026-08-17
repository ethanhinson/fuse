package permissions

import (
	"context"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/model"
)

// recordDecisions installs a recording DecisionSink on a fresh context and
// returns the context plus the recorded slice pointer.
func recordDecisions() (context.Context, *[]Decision) {
	rec := &[]Decision{}
	ctx := WithDecisionSink(context.Background(), func(d Decision) {
		*rec = append(*rec, d)
	})
	return ctx, rec
}

// TestDecisionSink_TerminalOutcomes drives one scenario per gate layer and
// asserts the emitted Decision names the right verdict and layer (change 0067).
func TestDecisionSink_TerminalOutcomes(t *testing.T) {
	root := t.TempDir()

	t.Run("rules deny", func(t *testing.T) {
		ctx, rec := recordDecisions()
		g := New(autoCfg(config.AutoConfig{}, nil, nil), newTestRegistry("bash"), AlwaysApprove,
			WithWorkspaceRoot(root))
		res := g.Execute(ctx, "bash", bashArgs("sudoedit /etc/hosts"))
		if !res.IsError || !res.Denied || res.DenyLayer != LayerRules {
			t.Fatalf("want typed rules denial, got %+v", res)
		}
		if len(*rec) != 1 || (*rec)[0].Verdict != "deny" || (*rec)[0].Layer != LayerRules {
			t.Fatalf("want one deny/rules decision, got %+v", *rec)
		}
		if (*rec)[0].Mode != "auto" || (*rec)[0].Command != "sudoedit /etc/hosts" {
			t.Errorf("decision should carry mode and command preview, got %+v", (*rec)[0])
		}
	})

	t.Run("safelist allow", func(t *testing.T) {
		ctx, rec := recordDecisions()
		g := New(autoCfg(config.AutoConfig{}, nil, nil), newTestRegistry("bash"), AlwaysApprove,
			WithWorkspaceRoot(root))
		res := g.Execute(ctx, "bash", bashArgs("git status"))
		if res.IsError {
			t.Fatalf("read-only should execute, got %+v", res)
		}
		if len(*rec) != 1 || (*rec)[0].Verdict != "allow" {
			t.Fatalf("want one allow decision, got %+v", *rec)
		}
		// "git status" auto-approves via the rules layer's per-segment allow or the
		// read-only safe list depending on config; either deterministic layer is
		// acceptable, but it must NOT be classifier/human.
		if l := (*rec)[0].Layer; l != LayerSafelist && l != LayerRules {
			t.Errorf("read-only allow decided by %q, want a deterministic layer", l)
		}
	})

	t.Run("mode off allow", func(t *testing.T) {
		ctx, rec := recordDecisions()
		g := New(config.PermissionsConfig{Mode: "off"}, newTestRegistry("bash"), AlwaysApprove)
		g.Execute(ctx, "bash", bashArgs("anything at all"))
		if len(*rec) != 1 || (*rec)[0].Layer != LayerModeOff || (*rec)[0].Verdict != "allow" {
			t.Fatalf("want mode_off allow, got %+v", *rec)
		}
	})

	t.Run("disabled deny", func(t *testing.T) {
		ctx, rec := recordDecisions()
		g := New(config.PermissionsConfig{Mode: "smart", Disabled: []string{"bash"}},
			newTestRegistry("bash"), AlwaysApprove)
		res := g.Execute(ctx, "bash", bashArgs("ls"))
		if !res.Denied || res.DenyLayer != LayerDisabled {
			t.Fatalf("want typed disabled denial, got %+v", res)
		}
		if len(*rec) != 1 || (*rec)[0].Layer != LayerDisabled {
			t.Fatalf("want disabled decision, got %+v", *rec)
		}
	})

	t.Run("ask then human outcome (AlwaysApprove shows ask→allow)", func(t *testing.T) {
		ctx, rec := recordDecisions()
		// prompt-all: every call routes to the human. AlwaysApprove stands in for
		// the headless binding — the event stream must still show the ask.
		g := New(config.PermissionsConfig{Mode: "prompt-all"}, newTestRegistry("bash"), AlwaysApprove)
		res := g.Execute(ctx, "bash", bashArgs("ls"))
		if res.IsError {
			t.Fatalf("approved call should execute, got %+v", res)
		}
		if len(*rec) != 2 {
			t.Fatalf("want ask + human decisions, got %+v", *rec)
		}
		if (*rec)[0].Verdict != "ask" {
			t.Errorf("first decision must be the pre-approval ask, got %+v", (*rec)[0])
		}
		if (*rec)[1].Verdict != "allow" || (*rec)[1].Layer != LayerHuman {
			t.Errorf("second decision must be the human allow, got %+v", (*rec)[1])
		}
	})

	t.Run("human deny is typed", func(t *testing.T) {
		ctx, rec := recordDecisions()
		deny := func(context.Context, ApprovalRequest) (bool, bool, error) { return false, false, nil }
		g := New(config.PermissionsConfig{Mode: "prompt-all"}, newTestRegistry("bash"), deny)
		res := g.Execute(ctx, "bash", bashArgs("ls"))
		if !res.Denied || res.DenyLayer != LayerHuman {
			t.Fatalf("want typed human denial, got %+v", res)
		}
		if !strings.Contains(res.Output, "tool call denied by user") {
			t.Errorf("human denial keeps its message, got %q", res.Output)
		}
		if len(*rec) != 2 || (*rec)[1].Verdict != "deny" || (*rec)[1].Layer != LayerHuman {
			t.Fatalf("want ask + human deny decisions, got %+v", *rec)
		}
	})

	t.Run("no sink is a no-op", func(t *testing.T) {
		g := New(autoCfg(config.AutoConfig{}, nil, nil), newTestRegistry("bash"), AlwaysApprove,
			WithWorkspaceRoot(root))
		res := g.Execute(context.Background(), "bash", bashArgs("git status"))
		if res.IsError {
			t.Fatalf("no-sink execution must be unaffected, got %+v", res)
		}
	})
}

// TestDecisionSink_ClassifierAndParseLayers proves the classifier verdict and
// the unparseable-bash ask are attributed to their layers.
func TestDecisionSink_ClassifierAndParseLayers(t *testing.T) {
	t.Run("classifier deny", func(t *testing.T) {
		ctx, rec := recordDecisions()
		stub := &stubCompleter{resp: model.CompletionResp{Content: `{"verdict":"deny","reason":"x"}`}}
		g := New(autoCfg(config.AutoConfig{}, nil, nil), newTestRegistry("bash"), AlwaysApprove,
			WithWorkspaceRoot(t.TempDir()), WithClassifier(newTestClassifier(t, stub)))
		res := g.Execute(ctx, "bash", bashArgs(grayAreaCmd(0)))
		if !res.Denied || res.DenyLayer != LayerClassifier {
			t.Fatalf("want typed classifier denial, got %+v", res)
		}
		if len(*rec) != 1 || (*rec)[0].Layer != LayerClassifier || (*rec)[0].Verdict != "deny" {
			t.Fatalf("want classifier deny decision, got %+v", *rec)
		}
	})

	t.Run("unparseable bash asks via parse layer", func(t *testing.T) {
		ctx, rec := recordDecisions()
		g := New(autoCfg(config.AutoConfig{}, nil, nil), newTestRegistry("bash"), AlwaysApprove,
			WithWorkspaceRoot(t.TempDir()))
		g.Execute(ctx, "bash", bashArgs("echo $(whoami)"))
		if len(*rec) != 2 {
			t.Fatalf("want ask + human decisions, got %+v", *rec)
		}
		if (*rec)[0].Verdict != "ask" || (*rec)[0].Layer != LayerParse {
			t.Fatalf("unparseable bash must ask via the parse layer, got %+v", (*rec)[0])
		}
	})
}

// TestDenyHint_AppendedToDenials proves denial outputs carry actionable
// guidance so the model stops retrying verbatim.
func TestDenyHint_AppendedToDenials(t *testing.T) {
	g := New(autoCfg(config.AutoConfig{}, nil, nil), newTestRegistry("bash"), AlwaysApprove,
		WithWorkspaceRoot(t.TempDir()))
	res := g.Execute(context.Background(), "bash", bashArgs("sudoedit /etc/hosts"))
	if !strings.Contains(res.Output, "retrying the identical call will fail") {
		t.Errorf("rules-layer denial should carry the no-retry hint, got %q", res.Output)
	}
	if !strings.Contains(res.Output, "denied by auto-mode rules layer") {
		t.Errorf("denial must keep the layer-named reason, got %q", res.Output)
	}
}
