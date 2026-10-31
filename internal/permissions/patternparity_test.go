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
// always_prompt row.
func TestProtectivePatterns_SameVerdictInSmartAndAuto(t *testing.T) {
	for _, r := range protectiveParityRows {
		t.Run(r.desc, func(t *testing.T) {
			run := func(cfg config.PermissionsConfig) (prompted, failed bool) {
				approve, called := newApproveRecorder(false)
				g := New(cfg, newTestRegistry("bash"), approve, WithWorkspaceRoot(t.TempDir()))
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
		})
	}
}
