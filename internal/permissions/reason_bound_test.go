package permissions

import (
	"context"
	"os"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ethanhinson/fuse/internal/config"
)

// TestReasonCommand_BoundsAndRedacts pins the one bounding rule for command
// text echoed in a reason, and that it matches the structured preview exactly.
func TestReasonCommand_BoundsAndRedacts(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"curl https://u:p@h/x", "curl https://***@h/x"},
		{strings.Repeat("a", 10001), strings.Repeat("a", 200) + "…"},
		{"  ls  ", "ls"},
		{"ls -la", "ls -la"},
		// A 3-byte rune at bytes 199-201 straddles the cap: the cut backs off
		// to the rune start rather than splitting it.
		{strings.Repeat("a", 199) + "€" + "bbb", strings.Repeat("a", 199) + "…"},
	}
	for _, tc := range cases {
		got := reasonCommand(tc.in)
		if !utf8.ValidString(got) {
			t.Errorf("reasonCommand(%.40q) = %q is not valid UTF-8", tc.in, got)
		}
		if body := strings.TrimSuffix(got, "…"); len(body) > 200 {
			t.Errorf("reasonCommand(%.40q) body is %d bytes, want at most 200", tc.in, len(body))
		}
		if got != tc.want {
			t.Errorf("reasonCommand(%.40q) = %.60q, want %.60q", tc.in, got, tc.want)
		}
		if prev := commandPreview("bash", bashArgs(tc.in)); got != prev {
			t.Errorf("reasonCommand(%.40q) = %.60q drifted from commandPreview %.60q", tc.in, got, prev)
		}
	}
}

func TestClassifierDenyReason_BoundsOperand(t *testing.T) {
	if got, want := classifierDenyReason("curl https://u:tok@h/x", "x"),
		"denied by auto-mode classifier: curl https://***@h/x (x)"; got != want {
		t.Errorf("classifierDenyReason = %q, want %q", got, want)
	}
	if got, want := classifierDenyReason(strings.Repeat("a", 5000), ""),
		"denied by auto-mode classifier: "+strings.Repeat("a", 200)+"…"; got != want {
		t.Errorf("classifierDenyReason over-length = %.80q..., want %.80q...", got, want)
	}
}

// TestDenyReasons_UseBoundedCommand drives every reachable deny site that
// echoes a command through Execute and asserts the recorded reason, and the
// model-facing output built from it, carry only the bounded, redacted command.
func TestDenyReasons_UseBoundedCommand(t *testing.T) {
	long := "sudo " + strings.Repeat("a", 10000)
	catastrophic := "rm -rf / " + strings.Repeat("x ", 2500)
	smart := config.PermissionsConfig{Mode: "smart"}
	rows := []struct {
		desc     string
		perms    config.PermissionsConfig
		approver Approver
		command  string
		prefix   string
		layer    string
		url      bool
	}{
		{"auto unparseable over-length", autoCfg(config.AutoConfig{}, nil, nil), AlwaysApprove,
			long, unparseableDenyPrefix, LayerRules, false},
		{"auto parsed rules deny", autoCfg(config.AutoConfig{Deny: []string{"bash:curl*"}}, nil, nil), AlwaysApprove,
			"curl https://u:tok@h/x", "denied by auto-mode rules layer: ", LayerRules, true},
		{"auto unparseable rules deny", autoCfg(config.AutoConfig{Deny: []string{"bash:sudo*"}}, nil, nil), AlwaysApprove,
			"sudo curl https://u:tok@h/x", "denied by auto-mode rules layer: ", LayerRules, true},
		{"smart floor deny", smart, AlwaysApprove,
			catastrophic, catastrophicFloorDenyPrefix, LayerRules, false},
		{"smart unparseable deny", smart, AlwaysApprove,
			long, unparseableDenyPrefix, LayerRules, false},
	}
	for _, row := range rows {
		t.Run(row.desc, func(t *testing.T) {
			ctx, rec := recordDecisions()
			g := New(row.perms, newTestRegistry("bash"), row.approver, WithWorkspaceRoot(t.TempDir()))
			res := g.Execute(ctx, "bash", bashArgs(row.command))
			if len(*rec) != 1 || (*rec)[0].Verdict != "deny" || (*rec)[0].Layer != row.layer {
				t.Fatalf("want one deny/%s decision, got %+v", row.layer, *rec)
			}
			d := (*rec)[0]
			if !strings.HasPrefix(d.Reason, row.prefix) {
				t.Errorf("reason %.80q lacks prefix %q", d.Reason, row.prefix)
			}
			if max := len(row.prefix) + 200 + len("…"); len(d.Reason) > max {
				t.Errorf("reason length %d exceeds bound %d", len(d.Reason), max)
			}
			if strings.Contains(d.Reason, "tok") {
				t.Errorf("reason leaks credential: %q", d.Reason)
			}
			if row.url && !strings.Contains(d.Reason, "***@") {
				t.Errorf("reason missing redaction marker: %q", d.Reason)
			}
			if !res.Denied {
				t.Fatalf("want denied result, got %+v", res)
			}
			suffix := "; " + denyHint(res.DenyLayer)
			if !strings.HasSuffix(res.Output, suffix) {
				t.Fatalf("output %.80q lacks deny hint suffix %q", res.Output, suffix)
			}
			if got := strings.TrimSuffix(res.Output, suffix); got != d.Reason {
				t.Errorf("model-facing reason %.80q differs from recorded %.80q", got, d.Reason)
			}
		})
	}

	t.Run("human-bound smart floor ask is bounded", func(t *testing.T) {
		ctx, rec := recordDecisions()
		approve, _ := newApproveRecorder(false)
		g := New(smart, newTestRegistry("bash"), Human(approve), WithWorkspaceRoot(t.TempDir()))
		g.Execute(ctx, "bash", bashArgs(catastrophic))
		if len(*rec) == 0 {
			t.Fatal("no decision recorded")
		}
		d := (*rec)[0]
		if d.Verdict != "ask" || d.Layer != LayerRules {
			t.Fatalf("want first decision ask/rules, got %+v", d)
		}
		if !strings.HasPrefix(d.Reason, catastrophicAskPrefix) {
			t.Errorf("reason %.80q lacks prefix %q", d.Reason, catastrophicAskPrefix)
		}
		if max := len(catastrophicAskPrefix) + 200 + len("…"); len(d.Reason) > max {
			t.Errorf("ask reason length %d exceeds bound %d", len(d.Reason), max)
		}
	})
}

// TestGateReasonsNeverEchoRawCommand is a source guard: every reason in
// gate.go that echoes a command must go through reasonCommand, so plain
// concatenation of a raw command or operand is refused.
func TestGateReasonsNeverEchoRawCommand(t *testing.T) {
	src, err := os.ReadFile("gate.go")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`\+ (command|operand)\b`)
	for i, line := range strings.Split(string(src), "\n") {
		if re.MatchString(line) {
			t.Errorf("gate.go:%d concatenates a raw command into a reason: %s", i+1, strings.TrimSpace(line))
		}
	}
}

// TestApproverDenyReason pins that an approver deny names who answered, so the
// reason sentence agrees with decided_by: a policy stand-in is not a user.
func TestApproverDenyReason(t *testing.T) {
	if got := approverDenyReason(DecidedByPolicy); got != "tool call denied by policy approver" {
		t.Errorf("approverDenyReason(policy) = %q", got)
	}
	if got := approverDenyReason(DecidedByHuman); got != "tool call denied by user" {
		t.Errorf("approverDenyReason(human) = %q", got)
	}
	if got := approverDenyReason(""); got != "tool call denied by user" {
		t.Errorf("approverDenyReason(empty) = %q", got)
	}

	deny := func(context.Context, ApprovalRequest) (bool, bool, error) { return false, false, nil }
	cases := []struct {
		desc      string
		approver  Approver
		decidedBy string
		reason    string
	}{
		{"human deny", Human(deny), DecidedByHuman, "tool call denied by user"},
		{"policy deny", Policy(deny), DecidedByPolicy, "tool call denied by policy approver"},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			ctx, rec := recordDecisions()
			g := New(config.PermissionsConfig{Mode: "smart"}, newTestRegistry("bash"), tc.approver, WithWorkspaceRoot(t.TempDir()))
			res := g.Execute(ctx, "bash", bashArgs("make build"))
			if len(*rec) == 0 {
				t.Fatal("no decisions recorded")
			}
			last := (*rec)[len(*rec)-1]
			if last.Verdict != "deny" || last.Layer != LayerHuman || last.DecidedBy != tc.decidedBy || last.Reason != tc.reason {
				t.Fatalf("last decision = %+v, want deny/%s/%s with reason %q", last, LayerHuman, tc.decidedBy, tc.reason)
			}
			if !strings.HasPrefix(res.Output, tc.reason) {
				t.Errorf("output = %q, want prefix %q", res.Output, tc.reason)
			}
			if res.DenyLayer != LayerHuman {
				t.Errorf("DenyLayer = %q, want %q", res.DenyLayer, LayerHuman)
			}
		})
	}
}

// TestDenyHint_Wording pins that the rules-layer hint describes the rules and
// that every hint is non-empty, dash-free, and warns about identical retries
// unless the layer names its own remedy.
func TestDenyHint_Wording(t *testing.T) {
	rules := denyHint(LayerRules)
	if !strings.Contains(rules, "blocked by the permission rules") || strings.Contains(rules, "catastrophic") {
		t.Fatalf("rules hint = %q", rules)
	}
	ownRemedy := map[string]bool{
		LayerHeuristic: true, LayerEditScope: true, LayerFetchFloor: true,
		LayerValve: true, LayerMediation: true,
	}
	layers := []string{
		LayerMediation, LayerDisabled, LayerModeOff, LayerCache, LayerParse,
		LayerRules, LayerSafelist, LayerHeuristic, LayerClassifier,
		LayerFetchFloor, LayerEditScope, LayerValve, LayerHuman,
		LayerSmartConfig, "",
	}
	for _, l := range layers {
		h := denyHint(l)
		if h == "" {
			t.Errorf("layer %q: empty hint", l)
		}
		if strings.ContainsRune(h, '—') {
			t.Errorf("layer %q: hint contains an em dash: %q", l, h)
		}
		if !ownRemedy[l] && !strings.Contains(h, "retrying the identical call will fail") {
			t.Errorf("layer %q: hint lacks retry warning: %q", l, h)
		}
	}
}
