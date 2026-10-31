package permissions

import (
	"context"
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
)

// protectiveParityRows is the one deny/ask table change 0088 runs through
// smart and auto mode. witness is an allow set that approves cmd in BOTH
// modes when no protective pattern is configured, so a prompt (or a deny)
// proves the protective pattern fired. Every cmd is off the read-only
// safelist, but that alone does not stop a later auto-mode layer from masking
// a default ask (one no explicit rule produced, which is not terminal):
// classifyHeuristic can still allow a mutation whose targets sit inside the
// workspace root. The ask survives here only because every cmd uses relative
// paths (a/b, ./pkg/dir), which resolve against the test process cwd (the
// package directory), and the gate's workspace root is a fresh t.TempDir()
// outside it. A row whose targets resolved inside that root could be allowed
// by the heuristic despite a protective ask hit.
var protectiveParityRows = []struct {
	desc    string
	pattern string
	cmd     string
	witness []string
	hit     bool
}{
	{"star spans a slash argument", "bash:rm *", "rm -rf a/b", []string{"bash:rm -rf a/*"}, true},
	{"star spans the slash segment of a compound", "bash:rm *", "git status && rm -rf a/b", []string{"bash:git status", "bash:rm -rf a/*"}, true},
	{"bare head matches in both modes", "bash:rm", "rm -rf a/b", []string{"bash:rm -rf a/*"}, true},
	{"bash:git * matches a slash-bearing git command", "bash:git *", "git commit -m x/y", []string{"bash:git commit -m x/*"}, true},
	{"trailing star spans a path", "bash:npm publish*", "npm publish ./pkg/dir", []string{"bash:npm publish ./pkg/*"}, true},
	{"class plus star", "bash:rm -[rR]f *", "rm -Rf a/b", []string{"bash:rm -Rf a/*"}, true},
	{"question mark matches a slash", "bash:rm -rf a?b", "rm -rf a/b", []string{"bash:rm -rf a/*"}, true},
	{"whole-command pattern", "bash:git status*rm -rf a/*", "git status && rm -rf a/b", []string{"bash:git status", "bash:rm -rf a/*"}, true},
	// Raw pre-peel subjects (change 0093): a protective rule naming a peeled
	// wrapper fires in both modes; the peeled subject still matches too.
	{"wrapper glob matches the command as written", "bash:timeout*", "timeout 5 rm x", []string{"bash:rm x"}, true},
	{"wrapper with argument text matches", "bash:nice *", "nice -n 5 make", []string{"bash:make"}, true},
	{"env wrapper with an inert assignment matches", "bash:env *", "env TZ=UTC make", []string{"bash:make"}, true},
	{"dash-c wrapper matches the outer call", "bash:bash *", `bash -c "make"`, []string{"bash:make"}, true},
	{"wrapper rule reaches a later segment", "bash:timeout*", "git status && timeout 5 rm x", []string{"bash:git status", "bash:rm x"}, true},
	{"peeled subject still matches under a wrapper", "bash:rm *", "timeout 5 rm x", []string{"bash:rm x"}, true},
	{"unrelated pattern misses", "bash:rm *", "mv a/b c/d", []string{"bash:mv a/* c/*"}, false},
	{"malformed pattern matches nothing", "bash:rm [", "rm -rf a/b", []string{"bash:rm -rf a/*"}, false},
}

// TestProtectivePatterns_SameMatchInSmartAndAuto checks the matcher layer:
// smart's protectivePattern (raw JSON args) and auto's protectiveHit (parsed
// segments) agree with each other and with the expected hit.
func TestProtectivePatterns_SameMatchInSmartAndAuto(t *testing.T) {
	for _, r := range protectiveParityRows {
		t.Run(r.desc, func(t *testing.T) {
			_, smart := protectivePattern([]string{r.pattern}, "bash", bashArgs(r.cmd))
			auto := protectiveHit([]string{r.pattern}, segsFor(t, r.cmd))
			if smart != r.hit || auto != r.hit {
				t.Errorf("%q vs %q: smart=%v auto=%v, want %v in both", r.pattern, r.cmd, smart, auto, r.hit)
			}
		})
	}
}

// TestProtectivePatterns_SameVerdictInSmartAndAuto is the end-to-end parity
// check through the real gate. For each row: with only the witness allow,
// neither mode prompts (the discriminator works); with the pattern as
// always_prompt, both modes prompt exactly when it hits, and auto.ask agrees;
// with the pattern as auto.deny, auto denies without a prompt exactly when it
// hits. Smart mode has no deny consumer, so its deny-shaped rule is the
// always_prompt row. A final policy pass binds only a stand-in approver: a hit
// is then the same rules-layer deny in both modes with identical reasons, and
// a miss is never a rules-layer deny.
func TestProtectivePatterns_SameVerdictInSmartAndAuto(t *testing.T) {
	for _, r := range protectiveParityRows {
		t.Run(r.desc, func(t *testing.T) {
			run := func(cfg config.PermissionsConfig) (prompted, failed bool) {
				approve, called := newApproveRecorder(false)
				g := New(cfg, newTestRegistry("bash"), Human(approve), WithWorkspaceRoot(t.TempDir()))
				res := g.Execute(context.Background(), "bash", bashArgs(r.cmd))
				return *called, res.IsError
			}
			smartCfg := func(alwaysPrompt []string) config.PermissionsConfig {
				return config.PermissionsConfig{Mode: "smart", AutoApprove: r.witness, AlwaysPrompt: alwaysPrompt}
			}

			if p, _ := run(smartCfg(nil)); p {
				t.Fatalf("baseline: smart witness %v must approve %q without a prompt", r.witness, r.cmd)
			}
			if p, _ := run(autoCfg(config.AutoConfig{}, r.witness, nil)); p {
				t.Fatalf("baseline: auto witness %v must approve %q without a prompt", r.witness, r.cmd)
			}

			smartPrompt, _ := run(smartCfg([]string{r.pattern}))
			autoPrompt, _ := run(autoCfg(config.AutoConfig{}, r.witness, []string{r.pattern}))
			autoAsk, _ := run(autoCfg(config.AutoConfig{Ask: []string{r.pattern}}, r.witness, nil))
			if smartPrompt != r.hit || autoPrompt != r.hit || autoAsk != r.hit {
				t.Errorf("ask parity for %q vs %q: smart always_prompt=%v auto always_prompt=%v auto.ask=%v, want %v",
					r.pattern, r.cmd, smartPrompt, autoPrompt, autoAsk, r.hit)
			}

			denyPrompt, denyFailed := run(autoCfg(config.AutoConfig{Deny: []string{r.pattern}}, r.witness, nil))
			if denyPrompt {
				t.Errorf("auto.deny %q must never route %q to the human", r.pattern, r.cmd)
			}
			if denyFailed != r.hit {
				t.Errorf("auto.deny %q on %q: denied=%v, want %v", r.pattern, r.cmd, denyFailed, r.hit)
			}

			// Policy pass (change 0092): with only a stand-in bound, a hit is the same
			// rules-layer deny in both modes, with identical reasons; a miss is never a
			// rules-layer deny.
			policyRun := func(cfg config.PermissionsConfig) []Decision {
				ctx, rec := recordDecisions()
				g := New(cfg, newTestRegistry("bash"), AlwaysApprove,
					WithWorkspaceRoot(t.TempDir()))
				g.Execute(ctx, "bash", bashArgs(r.cmd))
				return *rec
			}
			rulesDenies := func(ds []Decision) []Decision {
				var out []Decision
				for _, d := range ds {
					if d.Verdict == "deny" && d.Layer == LayerRules {
						out = append(out, d)
					}
				}
				return out
			}
			smartD := rulesDenies(policyRun(smartCfg([]string{r.pattern})))
			autoD := rulesDenies(policyRun(autoCfg(config.AutoConfig{}, r.witness, []string{r.pattern})))
			if r.hit {
				if len(smartD) != 1 || len(autoD) != 1 {
					t.Fatalf("policy pass: want one rules deny in each mode, smart=%+v auto=%+v", smartD, autoD)
				}
				if smartD[0].DecidedBy != DecidedByPolicy || autoD[0].DecidedBy != DecidedByPolicy {
					t.Errorf("policy pass: deny must be decided by policy, smart=%+v auto=%+v", smartD[0], autoD[0])
				}
				if smartD[0].Reason != autoD[0].Reason {
					t.Errorf("policy pass: reasons differ, smart=%q auto=%q", smartD[0].Reason, autoD[0].Reason)
				}
			} else if len(smartD) != 0 || len(autoD) != 0 {
				t.Errorf("policy pass: miss must not be a rules deny, smart=%+v auto=%+v", smartD, autoD)
			}
		})
	}
}

// unparseableProtectiveRows is the protective parity table for commands the
// shell parser refuses (change 0096). Both modes match them on the raw
// command text (rawCommandSubject) with the slash-spanning glob. They are not
// protectiveParityRows: that table's witness must approve cmd in auto mode,
// and auto mode has no allow for an unparseable command. smartWitness is the
// smart-mode legacy first-token allow (the boundary ADR-0068 pinned); nil
// means no allow in either mode. The stub registry means no row is executed.
var unparseableProtectiveRows = []struct {
	desc         string
	pattern      string
	cmd          string
	smartWitness []string
	hit          bool
}{
	{"wrapper glob reaches sudo as written", "bash:sudo*", "sudo ls", []string{"bash:sudo"}, true},
	{"slash-spanning glob reaches a path-qualified argv0", "bash:/bin/*", "/bin/rm -rf /", []string{"bash:/bin/rm"}, true},
	{"env rule reaches a non-inert assignment", "bash:env *", "env FOO=$(id) make", []string{"bash:env"}, true},
	{"npx rule reaches npx", "bash:npx *", "npx create-x", []string{"bash:npx"}, true},
	{"a rule naming the inner command does not reach a refused wrapper", "bash:rm *", "sudo rm -rf /", nil, false},
}

// TestProtectivePatterns_Unparseable_SameMatchInSmartAndAuto checks the
// matcher layer: smart's protectivePattern and auto's evalUnparseable agree on
// the hit, and on the pattern they name.
func TestProtectivePatterns_Unparseable_SameMatchInSmartAndAuto(t *testing.T) {
	for _, r := range unparseableProtectiveRows {
		t.Run(r.desc, func(t *testing.T) {
			if _, err := splitSegments(r.cmd); err == nil {
				t.Fatalf("fixture %q must be refused by the parser", r.cmd)
			}
			smartPat, smart := protectivePattern([]string{r.pattern}, "bash", bashArgs(r.cmd))
			askV, autoPat := evalUnparseable(r.cmd, config.AutoConfig{Ask: []string{r.pattern}}, nil)
			auto := askV == VerdictAskExplicit
			if smart != r.hit || auto != r.hit {
				t.Fatalf("%q vs %q: smart=%v auto=%v, want %v in both", r.pattern, r.cmd, smart, auto, r.hit)
			}
			if r.hit && (smartPat != r.pattern || autoPat != r.pattern) {
				t.Errorf("named patterns: smart=%q auto=%q, want %q", smartPat, autoPat, r.pattern)
			}
			if denyV, _ := evalUnparseable(r.cmd, config.AutoConfig{Deny: []string{r.pattern}}, nil); (denyV == VerdictDeny) != r.hit {
				t.Errorf("auto.deny %q on %q: verdict %v, want deny=%v", r.pattern, r.cmd, denyV, r.hit)
			}
		})
	}
}

// TestProtectivePatterns_Unparseable_SameVerdictInSmartAndAuto is the
// end-to-end parity check through the real gate. Human bound: a hit is an
// explicit ask at the rules layer naming the pattern in every rule slot of both
// modes, a miss is the parse-layer ask; auto.deny denies without a prompt
// exactly on a hit. Policy bound: both modes give one rules-layer deny decided
// by policy with identical reasons, the explicit-ask reason on a hit and the
// unparseable reason on a miss.
func TestProtectivePatterns_Unparseable_SameVerdictInSmartAndAuto(t *testing.T) {
	for _, r := range unparseableProtectiveRows {
		t.Run(r.desc, func(t *testing.T) {
			human := func(cfg config.PermissionsConfig) (Decision, bool) {
				t.Helper()
				ctx, rec := recordDecisions()
				approve, called := newApproveRecorder(false)
				g := New(cfg, newTestRegistry("bash"), Human(approve), WithWorkspaceRoot(t.TempDir()))
				g.Execute(ctx, "bash", bashArgs(r.cmd))
				if len(*rec) == 0 {
					t.Fatalf("no decision recorded for %q", r.cmd)
				}
				return (*rec)[0], *called
			}
			policy := func(cfg config.PermissionsConfig) []Decision {
				ctx, rec := recordDecisions()
				g := New(cfg, newTestRegistry("bash"), AlwaysApprove, WithWorkspaceRoot(t.TempDir()))
				g.Execute(ctx, "bash", bashArgs(r.cmd))
				return *rec
			}
			smartCfg := func(alwaysPrompt []string) config.PermissionsConfig {
				return config.PermissionsConfig{Mode: "smart", AutoApprove: r.smartWitness, AlwaysPrompt: alwaysPrompt}
			}

			if len(r.smartWitness) > 0 {
				if d, prompted := human(smartCfg(nil)); prompted || d.Verdict != "allow" {
					t.Fatalf("baseline: smart witness %v must approve %q without a prompt, got %+v", r.smartWitness, r.cmd, d)
				}
			}

			wantLayer, wantReason := LayerParse, ""
			if r.hit {
				wantLayer, wantReason = LayerRules, explicitAskReason(r.pattern, false)
			}
			for _, slot := range []struct {
				name string
				cfg  config.PermissionsConfig
			}{
				{"smart always_prompt", smartCfg([]string{r.pattern})},
				{"auto always_prompt", autoCfg(config.AutoConfig{}, nil, []string{r.pattern})},
				{"auto.ask", autoCfg(config.AutoConfig{Ask: []string{r.pattern}}, nil, nil)},
			} {
				d, prompted := human(slot.cfg)
				if !prompted || d.Verdict != "ask" || d.Layer != wantLayer || d.Reason != wantReason {
					t.Errorf("%s: got %+v prompted=%v, want ask at %s with reason %q", slot.name, d, prompted, wantLayer, wantReason)
				}
			}

			d, prompted := human(autoCfg(config.AutoConfig{Deny: []string{r.pattern}}, nil, nil))
			if r.hit && (prompted || d.Verdict != "deny" || d.Layer != LayerRules || d.Reason != "denied by auto-mode rules layer: "+r.cmd) {
				t.Errorf("auto.deny hit: got %+v prompted=%v, want a rules deny without a prompt", d, prompted)
			}
			if !r.hit && (!prompted || d.Layer != LayerParse) {
				t.Errorf("auto.deny miss: got %+v prompted=%v, want the parse-layer ask", d, prompted)
			}

			wantPolicy := unparseableDenyPrefix + r.cmd
			if r.hit {
				wantPolicy = explicitAskReason(r.pattern, true)
			}
			smartD := policy(smartCfg([]string{r.pattern}))
			autoD := policy(autoCfg(config.AutoConfig{}, nil, []string{r.pattern}))
			for name, ds := range map[string][]Decision{"smart": smartD, "auto": autoD} {
				if len(ds) != 1 || ds[0].Verdict != "deny" || ds[0].Layer != LayerRules || ds[0].DecidedBy != DecidedByPolicy {
					t.Errorf("policy %s: want one rules deny decided by policy, got %+v", name, ds)
					continue
				}
				if ds[0].Reason != wantPolicy {
					t.Errorf("policy %s: reason %q, want %q", name, ds[0].Reason, wantPolicy)
				}
			}
		})
	}
}
