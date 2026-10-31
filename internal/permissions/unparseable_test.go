package permissions

import (
	"context"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
)

// unparseableRows are bash commands the shell parser refuses (change 0096),
// one per refusal shape. None is ever executed: every gate in this file wraps
// the stub registry from newTestRegistry.
var unparseableRows = []struct{ desc, cmd string }{
	{"arbitrary-arg wrapper sudo", "sudo rm -rf /"},
	{"absolute path-qualified argv0", "/bin/rm -rf /"},
	{"relative path-qualified argv0", "./rm -rf x"},
	{"non-inert env assignment prefix", "FOO=$(id) make"},
	{"arbitrary-arg wrapper npx", "npx evil"},
	{"arbitrary-arg wrapper xargs", "xargs rm"},
	{"opaque command word", "$CMD x"},
	{"bash without -c", "bash script.sh"},
	{"process substitution", "make <(ls)"},
	// "echo " (5 bytes) plus maxCommandBytes-4 bytes is one byte over the cap.
	{"command over the length cap", "echo " + strings.Repeat("a", maxCommandBytes-4)},
}

// failIfCalled is an approver body that fails the test when the gate consults
// it: under a policy stand-in an unparseable command is decided before any
// approver runs.
func failIfCalled(t *testing.T) ApprovalFunc {
	return func(context.Context, ApprovalRequest) (bool, bool, error) {
		t.Errorf("the approver must not be consulted")
		return true, false, nil
	}
}

// TestUnparseableRows_AreRefused guards the fixtures: every row must really be
// refused by the parser, or the tests below prove nothing.
func TestUnparseableRows_AreRefused(t *testing.T) {
	for _, r := range unparseableRows {
		if _, err := splitSegments(r.cmd); err == nil {
			t.Errorf("fixture %q must be refused by splitSegments", r.desc)
		}
	}
}

// TestAutoMode_Unparseable_PolicyDenies: with only a policy stand-in bound, a
// command the parser refuses is a rules-layer deny decided by policy, and the
// stand-in is never asked (change 0096).
func TestAutoMode_Unparseable_PolicyDenies(t *testing.T) {
	for _, r := range unparseableRows {
		t.Run(r.desc, func(t *testing.T) {
			ctx, rec := recordDecisions()
			g := New(autoCfg(config.AutoConfig{}, nil, nil), newTestRegistry("bash"), Policy(failIfCalled(t)),
				WithWorkspaceRoot(t.TempDir()))
			res := g.Execute(ctx, "bash", bashArgs(r.cmd))
			if !res.IsError || !res.Denied || res.DenyLayer != LayerRules {
				t.Fatalf("want a typed rules-layer denial, got %+v", res)
			}
			if len(*rec) != 1 {
				t.Fatalf("want exactly one decision (no ask, no allow), got %+v", *rec)
			}
			d := (*rec)[0]
			if d.Verdict != "deny" || d.Layer != LayerRules || d.DecidedBy != DecidedByPolicy || d.Mode != "auto" {
				t.Errorf("want deny/rules/policy/auto, got %+v", d)
			}
			if want := unparseableDenyPrefix + r.cmd; d.Reason != want {
				t.Errorf("reason = %q, want %q", d.Reason, want)
			}
		})
	}
}

// TestAutoMode_Unparseable_HumanStillAsks pins the attended path: with a human
// bound the events are exactly what they were before change 0096, an ask at
// the parse layer with no reason, then the human's allow.
func TestAutoMode_Unparseable_HumanStillAsks(t *testing.T) {
	for _, r := range unparseableRows {
		t.Run(r.desc, func(t *testing.T) {
			ctx, rec := recordDecisions()
			calls := 0
			approve := func(context.Context, ApprovalRequest) (bool, bool, error) {
				calls++
				return true, false, nil
			}
			g := New(autoCfg(config.AutoConfig{}, nil, nil), newTestRegistry("bash"), Human(approve),
				WithWorkspaceRoot(t.TempDir()))
			res := g.Execute(ctx, "bash", bashArgs(r.cmd))
			if res.IsError {
				t.Fatalf("a human-approved call must run, got %+v", res)
			}
			if calls != 1 {
				t.Fatalf("approver called %d times, want 1", calls)
			}
			if len(*rec) != 2 {
				t.Fatalf("want ask then allow, got %+v", *rec)
			}
			ask, allow := (*rec)[0], (*rec)[1]
			if ask.Verdict != "ask" || ask.Layer != LayerParse || ask.Reason != "" || ask.DecidedBy != "" {
				t.Errorf("want ask/parse with no reason, got %+v", ask)
			}
			if allow.Verdict != "allow" || allow.Layer != LayerHuman || allow.DecidedBy != DecidedByHuman {
				t.Errorf("want allow/human/human, got %+v", allow)
			}
		})
	}
}

// TestAutoMode_Unparseable_ProtectiveRulesSeeRawText: auto mode's protective
// rules match the raw text of a refused command, in both provenances, with the
// same reasons as for a parsed command (change 0096).
func TestAutoMode_Unparseable_ProtectiveRulesSeeRawText(t *testing.T) {
	type want struct {
		verdict, layer, reason, decidedBy string
		prompted                          bool
	}
	cases := []struct {
		desc         string
		auto         config.AutoConfig
		autoApprove  []string
		alwaysPrompt []string
		cmd          string
		human        want
		policy       want
	}{
		{
			desc: "auto.deny naming the wrapper is a plain rule deny",
			auto: config.AutoConfig{Deny: []string{"bash:sudo*"}}, cmd: "sudo ls",
			human:  want{"deny", LayerRules, "denied by auto-mode rules layer: sudo ls", "", false},
			policy: want{"deny", LayerRules, "denied by auto-mode rules layer: sudo ls", "", false},
		},
		{
			desc: "auto.ask spans the slash of a path-qualified argv0",
			auto: config.AutoConfig{Ask: []string{"bash:/bin/*"}}, cmd: "/bin/rm -rf /",
			human:  want{"ask", LayerRules, "matched ask rule bash:/bin/*", "", true},
			policy: want{"deny", LayerRules, "matched ask rule bash:/bin/*; no approver bound", DecidedByPolicy, false},
		},
		{
			desc:         "always_prompt reaches a non-inert env prefix",
			alwaysPrompt: []string{"bash:env *"}, cmd: "env FOO=$(id) make",
			human:  want{"ask", LayerRules, "matched ask rule bash:env *", "", true},
			policy: want{"deny", LayerRules, "matched ask rule bash:env *; no approver bound", DecidedByPolicy, false},
		},
		{
			desc: "a rule naming the inner command does not peel a refused wrapper",
			auto: config.AutoConfig{Deny: []string{"bash:rm *"}}, cmd: "sudo rm -rf /",
			human:  want{"ask", LayerParse, "", "", true},
			policy: want{"deny", LayerRules, unparseableDenyPrefix + "sudo rm -rf /", DecidedByPolicy, false},
		},
		{
			desc:        "auto_approve never admits an unparseable command in auto mode",
			autoApprove: []string{"bash:sudo"}, cmd: "sudo ls",
			human:  want{"ask", LayerParse, "", "", true},
			policy: want{"deny", LayerRules, unparseableDenyPrefix + "sudo ls", DecidedByPolicy, false},
		},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			check := func(name string, a Approver, called *bool, w want) {
				t.Helper()
				ctx, rec := recordDecisions()
				g := New(autoCfg(tc.auto, tc.autoApprove, tc.alwaysPrompt), newTestRegistry("bash"), a,
					WithWorkspaceRoot(t.TempDir()))
				g.Execute(ctx, "bash", bashArgs(tc.cmd))
				if len(*rec) == 0 {
					t.Fatalf("%s: no decision recorded", name)
				}
				d := (*rec)[0]
				if d.Verdict != w.verdict || d.Layer != w.layer || d.Reason != w.reason || d.DecidedBy != w.decidedBy {
					t.Errorf("%s: first decision = %+v, want %s/%s reason %q decided_by %q",
						name, d, w.verdict, w.layer, w.reason, w.decidedBy)
				}
				if *called != w.prompted {
					t.Errorf("%s: approver called = %v, want %v", name, *called, w.prompted)
				}
				for _, d := range *rec {
					if d.Verdict == "allow" {
						t.Errorf("%s: no allow expected, got %+v", name, d)
					}
				}
			}
			// The human declines, so a prompt never turns into an allow; the
			// policy stand-in would approve, so any allow is a regression.
			humanFn, humanCalled := newApproveRecorder(false)
			check("human", Human(humanFn), humanCalled, tc.human)
			policyFn, policyCalled := newApproveRecorder(true)
			check("policy", Policy(policyFn), policyCalled, tc.policy)
		})
	}
}
