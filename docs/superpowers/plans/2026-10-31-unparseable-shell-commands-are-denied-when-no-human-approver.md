<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0096 — Unparseable shell commands are denied when no human approver is bound](../../changes/active/0096-unparseable-shell-commands-are-denied-when-no-human-approver.md)**
<!-- docket:backlink:end -->

# Unparseable shell commands are denied when no human approver is bound: Implementation Plan

> Executed by `docket-build`: each `### Task N` heading below is one worker, one commit. The
> closing ADR section is a coordinator step and is deliberately not a `### Task` heading.

**Goal:** A bash command the shell parser refuses is never answered by a policy stand-in. In auto and
smart mode, with only a policy stand-in bound, it is denied at the rules layer with
`unparseable command denied; no approver bound: <command>`. With a human bound it still prompts. In
auto mode, protective rules (`auto.deny`, `auto.ask`, `always_prompt`) now match the raw command text
of an unparseable command, as smart mode already does.

**Spec:** `docs/superpowers/specs/2026-10-31-unparseable-shell-commands-are-denied-when-no-human-approver-design.md`
(feature worktree). Read its Design, Testing and Compatibility sections before starting any task.

**Architecture:** Two small helpers carry the shared behavior: `rawCommandSubject` (patterns.go) is
the one raw protective subject for a refused command, and `evalUnparseable` (rules.go) is the
rules layer for one (deny, then ask, never allow). `resolveAuto` replaces its parse-failure early
return with `evalUnparseable` plus a provenance check. The smart branch of `resolve` adds one check
after its allow step misses. Provenance is `g.approvalProvenance` (from change 0094:
`permissions.Approver{Answer, DecidedBy}`; `Human(fn)` is human, `Policy(fn)` and `AlwaysApprove`
are policy).

**Tech stack:** Go, `internal/permissions`, `cmd/fuse` composition-root tests, README.

## Ground rules for every task

- **Host safety.** Never execute a destructive command. Gate tests in `internal/permissions` wrap
  `newTestRegistry("bash")` (a stub that runs nothing), so `sudo rm -rf /` rows are safe there and
  only there. The only command any test sends to a real bash is `/usr/bin/touch ran` inside a temp
  workspace (`floorWorkspace`). Do not run any of the fixture commands in a shell yourself.
- **No live model traffic.** Use the scripted gateway double (`recordingGateway`,
  `scriptedBashCall`, `scriptedFinal`) only. Never point a test at a real provider, and never at
  Anthropic.
- **Writing style.** No em dashes in code, comments, test names, docs or commit messages. Do not add
  co-author or attribution trailers to commits.
- **Commit messages** use the repo's conventional style, for example
  `fix(permissions): deny unparseable bash under a policy stand-in in auto mode`.
- **Formatting:** code blocks inside list items below carry markdown indentation; run
  `gofmt -l internal/permissions cmd/fuse` and fix anything it lists before committing.
- **Verification per task:** `go build ./...`, `go vet ./internal/permissions/ ./cmd/fuse/`, and the
  focused `go test -run` named in the task, plus `go test ./internal/permissions/` for every task that
  touches that package.
- **Known environmental failure.** `TestSessionModeFlipToAuto_NextGateAutoApprovesReadOnlyBash` and
  `TestBuildGate_MidTurnFlipBitesSameGate` in `cmd/fuse` run `git status` and fail only in a copy
  of the tree with no `.git`. In the feature worktree they pass; they are unrelated to this change.

## Prototype evidence

The plan writer applied the Task 1 to Task 3 implementation to a throwaway copy of the tree at the
pre-dispatch HEAD (6245ccf) and ran the suite. Results the build can rely on:

- Exactly three existing tests encode the old "policy approves an unparseable command" behavior and
  fail after Task 2: `TestAutoMode_RedirectWriteTargets_EndToEnd` (rows `echo x > $F`,
  `cat <> scratch`), `TestAutoMode_OpaqueArgs_EndToEnd` (five `${...$(...)...}` rows), and the
  `unparseable bash asks via parse layer` subtest of `TestDecisionSink_ClassifierAndParseLayers`.
  Each is about the parse floor, not about the stand-in, so Task 2 binds a human approver in them;
  no assertion is weakened. No other test in the repository fails.
- Every fixture command in this plan is refused by `splitSegments` (including `sudo ls`,
  `npx create-x`, `env FOO=$(id) make` and `/usr/bin/touch ran`), and every expected decision in the
  tests below was observed exactly on the prototype, including the end-to-end loop-server runs.

## Deviations from the spec (deliberate)

1. **Parity rows get their own table.** The spec puts the unparseable rows in
   `protectiveParityRows` and says the two parity tests run them unchanged. They cannot: that table's
   witness must approve the command in auto mode, and auto mode has no allow for an unparseable
   command (by design), so the baseline assertion would fail; and its policy pass asserts that a miss
   is never a rules-layer deny, which this change makes false on purpose. Task 4 adds
   `unparseableProtectiveRows` with the spec's five rows and two sibling tests that make the same
   claims (both modes agree on the hit and on the policy-provenance deny reason).
2. **No `parsedBash` memo.** The spec says behavior, not performance, is the goal and not to
   restructure the three helpers. Task 3 adds one `splitSegments` call on the smart path that has
   already missed the allow step (bounded by `maxCommandBytes`), through a small `unparseableBash`
   helper, and adds no memo.
3. **Three existing tests change their approver** (see Prototype evidence). The spec lists the
   existing fixtures that pass unchanged; these three were not on its list.

---

### Task 1: Raw protective subject and the unparseable rules layer

**Build tier:** standard

**Files:**
- Modify: `internal/permissions/patterns.go` (`protectivePattern`, new `rawCommandSubject`)
- Modify: `internal/permissions/rules.go` (new `evalUnparseable`)
- Test: `internal/permissions/patterns_test.go`, `internal/permissions/rules_test.go`

- [ ] **Step 1: Write the failing tests.**

Append to `internal/permissions/patterns_test.go` (it already imports `testing`):

```go
// TestRawCommandSubject_Normalises pins the one raw protective subject for a
// command the parser refuses (change 0096): "bash:" plus the command's words
// joined by single spaces, so smart and auto mode normalise identically.
func TestRawCommandSubject_Normalises(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"  sudo   ls\t", "bash:sudo ls", true},
		{"sudo\nls", "bash:sudo ls", true},
		{"/bin/rm -rf /", "bash:/bin/rm -rf /", true},
		{"", "", false},
		{"   ", "", false},
	}
	for _, tc := range cases {
		got, ok := rawCommandSubject(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("rawCommandSubject(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
	// protectivePattern goes through the same normalisation: a pattern written
	// with single spaces reaches a refused command written with several.
	if pat, hit := protectivePattern([]string{"bash:sudo ls"}, "bash", bashArgs("sudo   ls")); !hit || pat != "bash:sudo ls" {
		t.Errorf("protectivePattern on a multi-space refused command = (%q, %v), want (bash:sudo ls, true)", pat, hit)
	}
}
```

Append to `internal/permissions/rules_test.go` (it already imports `config` and `testing`):

```go
// TestEvalUnparseable_DenyBeatsAsk: a deny rule wins over an ask rule on the
// raw text of a command the parser refuses (change 0096).
func TestEvalUnparseable_DenyBeatsAsk(t *testing.T) {
	cfg := config.AutoConfig{Deny: []string{"bash:sudo*"}, Ask: []string{"bash:sudo*"}}
	if v, _ := evalUnparseable("sudo ls", cfg, []string{"bash:sudo*"}); v != VerdictDeny {
		t.Fatalf("evalUnparseable = %v, want deny", v)
	}
}

// TestEvalUnparseable_Precedence covers the rest of the raw-text rules layer:
// ask then always_prompt (in that order) name their pattern, the matcher spans
// '/', a rule naming the inner command does not peel a refused wrapper, and
// with no hit the verdict is the default ask (never allow).
func TestEvalUnparseable_Precedence(t *testing.T) {
	cases := []struct {
		desc         string
		cfg          config.AutoConfig
		alwaysPrompt []string
		cmd          string
		want         Verdict
		wantPattern  string
	}{
		{"no rules is the default ask", config.AutoConfig{}, nil, "sudo ls", VerdictAsk, ""},
		{"auto.ask matches the raw text", config.AutoConfig{Ask: []string{"bash:sudo*"}}, nil, "sudo ls", VerdictAskExplicit, "bash:sudo*"},
		{"always_prompt matches the raw text", config.AutoConfig{}, []string{"bash:env *"}, "env FOO=$(id) make", VerdictAskExplicit, "bash:env *"},
		{"auto.ask is consulted before always_prompt", config.AutoConfig{Ask: []string{"bash:npx *"}}, []string{"bash:npx*"}, "npx create-x", VerdictAskExplicit, "bash:npx *"},
		{"the protective star spans a slash", config.AutoConfig{Ask: []string{"bash:/bin/*"}}, nil, "/bin/rm -rf /", VerdictAskExplicit, "bash:/bin/*"},
		{"a deny naming the inner command does not peel the wrapper", config.AutoConfig{Deny: []string{"bash:rm *"}}, nil, "sudo rm -rf /", VerdictAsk, ""},
		{"an empty command matches nothing", config.AutoConfig{Deny: []string{"bash:*"}}, nil, "   ", VerdictAsk, ""},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			got, pat := evalUnparseable(tc.cmd, tc.cfg, tc.alwaysPrompt)
			if got != tc.want || pat != tc.wantPattern {
				t.Errorf("evalUnparseable(%q) = (%v, %q), want (%v, %q)", tc.cmd, got, pat, tc.want, tc.wantPattern)
			}
		})
	}
}
```

- [ ] **Step 2: Run them and confirm they fail to compile** (undefined `rawCommandSubject`,
  `evalUnparseable`):
  `go test ./internal/permissions/ -run 'TestRawCommandSubject_Normalises|TestEvalUnparseable_'`

- [ ] **Step 3: Implement.**

In `internal/permissions/patterns.go`, replace the parse-failure fallback inside `protectivePattern`:

```go
	if err != nil {
		raw := strings.TrimSpace(command)
		if raw == "" {
			return "", false
		}
		return firstProtectiveMatch(patterns, "bash:"+raw)
	}
```

with:

```go
	if err != nil {
		subj, ok := rawCommandSubject(command)
		if !ok {
			return "", false
		}
		return firstProtectiveMatch(patterns, subj)
	}
```

In the `protectivePattern` doc comment, change "A command the parser refuses is matched on its raw
text instead, so a protective rule still reaches it." to "A command the parser refuses is matched on
its raw text (rawCommandSubject) instead, so a protective rule still reaches it; auto mode's
evalUnparseable uses the same subject (change 0096)."

Add, directly after `protectivePattern`:

```go
// rawCommandSubject is the one protective subject for a bash command the
// parser refuses (change 0096): "bash:" plus the command's words joined by
// single spaces. Smart mode (protectivePattern) and auto mode
// (evalUnparseable) both use it, so the two modes normalise identically. It
// reports false for an empty or all-whitespace command.
func rawCommandSubject(command string) (string, bool) {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return "", false
	}
	return "bash:" + strings.Join(fields, " "), true
}
```

In `internal/permissions/rules.go`, add directly after `evalRulesDetail`:

```go
// evalUnparseable is the rules layer for a bash command the parser refuses
// (change 0096). There are no segments, so the protective rules are matched
// against the raw command text (rawCommandSubject) with matchProtective, whose
// star spans '/': deny first (cfg.Deny gives VerdictDeny), then ask (cfg.Ask,
// then alwaysPrompt, gives VerdictAskExplicit and the matched pattern), else
// the default VerdictAsk. There is no allow step: an unparseable command has no
// segment to satisfy allSegmentsAllowed, so no permissive rule admits it in
// auto mode. A rule naming the inner command ("bash:rm *") does not reach
// "sudo rm -rf /": raw matching is not a peel.
func evalUnparseable(command string, cfg AutoConfig, alwaysPrompt []string) (Verdict, string) {
	subj, ok := rawCommandSubject(command)
	if !ok {
		return VerdictAsk, ""
	}
	if _, hit := firstProtectiveMatch(cfg.Deny, subj); hit {
		return VerdictDeny, ""
	}
	askPatterns := append(append([]string(nil), cfg.Ask...), alwaysPrompt...)
	if pat, hit := firstProtectiveMatch(askPatterns, subj); hit {
		return VerdictAskExplicit, pat
	}
	return VerdictAsk, ""
}
```

- [ ] **Step 4: Run the focused tests, then the package.** Both commands from Step 2 and
  `go test ./internal/permissions/` pass. (`evalUnparseable` has no caller yet; that is Task 2.)

- [ ] **Step 5: Commit.** `feat(permissions): add the raw-text rules layer for unparseable bash`

---

### Task 2: Auto mode denies unparseable commands under a policy stand-in

**Build tier:** premium (a permission-gate change on the no-human path; a mistake is a silent
approval or a silent denial)

**Files:**
- Modify: `internal/permissions/gate.go` (`resolveAuto` step 1 and its doc comment; the auto
  `VerdictDeny` case in `resolve`; new `unparseableDenyPrefix` and `policyForcedDeny`)
- Modify: `internal/permissions/decision.go` (the `LayerParse` comment)
- Modify: `internal/permissions/auto_test.go`, `internal/permissions/decision_test.go` (three
  existing parse-floor tests bind a human approver)
- Create: `internal/permissions/unparseable_test.go`

- [ ] **Step 1: Write the failing tests.** Create `internal/permissions/unparseable_test.go`:

```go
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
```

- [ ] **Step 2: Run and confirm failure.**
  `go test ./internal/permissions/ -run 'TestUnparseableRows_AreRefused|TestAutoMode_Unparseable_'`
  Expect a compile failure on `unparseableDenyPrefix`. (Once it compiles, `_HumanStillAsks` and the
  fixture guard pass before the gate change; they pin unchanged behavior. `_PolicyDenies` and the
  raw-text cases fail.)

- [ ] **Step 3: Implement in `internal/permissions/gate.go`.**

Next to `explicitAskDenyPrefix`, add:

```go
// unparseableDenyPrefix opens the reason when a command the parser refuses
// reaches a gate with no human to ask (change 0096).
const unparseableDenyPrefix = "unparseable command denied; no approver bound: "

// policyForcedDeny reports whether a rules-layer deny reason is one only a
// policy stand-in produces (an explicit ask, the smart-mode catastrophic floor,
// or an unparseable command), so the decision records decided_by. A plain rule
// deny ("denied by auto-mode rules layer: ...") keeps decided_by empty.
func policyForcedDeny(reason string) bool {
	return strings.HasPrefix(reason, explicitAskDenyPrefix) ||
		strings.HasPrefix(reason, catastrophicFloorDenyPrefix) ||
		strings.HasPrefix(reason, unparseableDenyPrefix)
}
```

In `resolve`, auto `case VerdictDeny:`, replace

```go
			// A deny forced by a policy stand-in (explicit ask, no human bound)
			// records the provenance so the trail shows why rules denied.
			decidedBy := ""
			if layer == LayerRules && strings.HasPrefix(reason, explicitAskDenyPrefix) {
```

with

```go
			// A deny forced by a policy stand-in (an explicit ask or an
			// unparseable command, no human bound) records the provenance so the
			// trail shows why rules denied.
			decidedBy := ""
			if layer == LayerRules && policyForcedDeny(reason) {
```

In `resolveAuto`, replace the three-line comment that opens with
`// 1. Static floor: parse into segments. Unparseable fails toward the human.` together with the
code under it:

```go
	segments, err := splitSegments(command)
	if err != nil {
		return VerdictAsk, LayerParse, "", nil
	}
```

with

```go
	// 1. Static floor: parse into segments. A command the parser refuses has
	// no segment for any later layer, so evalUnparseable matches the protective
	// rules against its raw text (change 0096): a deny rule denies, an explicit
	// ask rule is its own verdict as below. Only a human may answer the rest:
	// with a policy stand-in bound it is denied at the rules layer, and with a
	// human bound it is the countable LayerParse ask it has always been (the
	// shapes change 0070 targets). Nothing after this step runs for it.
	segments, err := splitSegments(command)
	if err != nil {
		uv, pat := evalUnparseable(command, g.cfg.Auto, g.cfg.AlwaysPrompt)
		switch {
		case uv == VerdictDeny:
			return VerdictDeny, LayerRules, "denied by auto-mode rules layer: " + command, nil
		case uv == VerdictAskExplicit && g.approvalProvenance == DecidedByPolicy:
			return VerdictDeny, LayerRules, explicitAskReason(pat, true), nil
		case uv == VerdictAskExplicit:
			return VerdictAsk, LayerRules, explicitAskReason(pat, false), nil
		case g.approvalProvenance == DecidedByPolicy:
			return VerdictDeny, LayerRules, unparseableDenyPrefix + command, nil
		default:
			return VerdictAsk, LayerParse, "", nil
		}
	}
```

In the `resolveAuto` doc comment, replace step 1

```go
//  1. Split segments (bash only). ErrUnparseable ⇒ ask (fail toward the human;
//     an unparseable command is not routed to the classifier).
```

with

```go
//  1. Split segments (bash only). ErrUnparseable ⇒ evalUnparseable on the raw
//     text: Deny ⇒ deny; AskExplicit ⇒ ask (human) or deny (policy); no hit ⇒
//     ask at LayerParse (human) or deny at LayerRules (policy). An unparseable
//     command never reaches the safelist, heuristics or classifier (change 0096).
```

In `internal/permissions/decision.go`, change the `LayerParse` line comment to
`// unparseable bash with a human bound ⇒ fail-closed ask (the #0070 target); a policy stand-in gets a rules deny (change 0096)`.

- [ ] **Step 4: Rebind the three parse-floor tests to a human approver.** They assert the parse
  floor, and their gates use `AlwaysApprove`, which is now a rules-layer deny for those rows. Do not
  change any expected verdict or layer.
  - `internal/permissions/auto_test.go`, `TestAutoMode_RedirectWriteTargets_EndToEnd` (around line
    489) and `TestAutoMode_OpaqueArgs_EndToEnd` (around line 597): in each, insert before the
    `g := New(...)` line

    ```go
    			// A human approver keeps the parse-floor rows at LayerParse; under a
    			// policy stand-in they are rules-layer denies (change 0096,
    			// unparseable_test.go). resolveAuto never calls the approver.
    			approve, _ := newApproveRecorder(false)
    ```

    and replace `AlwaysApprove,` in that `New(...)` call with `Human(approve),`. Leave the other
    `AlwaysApprove` gates in auto_test.go (around lines 721, 745, 788) untouched.
  - `internal/permissions/decision_test.go`, subtest `"unparseable bash asks via parse layer"` of
    `TestDecisionSink_ClassifierAndParseLayers` (around line 147): insert
    `approve, _ := newApproveRecorder(true)` after `ctx, rec := recordDecisions()`, and replace
    `AlwaysApprove,` in its `New(...)` call with `Human(approve),`. Its two-decision assertion then
    holds as written (ask at parse, allow at human).

- [ ] **Step 5: Verify.**
  `go test ./internal/permissions/ -run 'TestUnparseableRows_AreRefused|TestAutoMode_Unparseable_|TestAutoMode_RedirectWriteTargets_EndToEnd|TestAutoMode_OpaqueArgs_EndToEnd|TestDecisionSink_'`
  then `go test ./internal/permissions/`. All pass.

- [ ] **Step 6: Commit.** `fix(permissions): deny unparseable bash under a policy stand-in in auto mode`

---

### Task 3: Smart mode denies unparseable commands under a policy stand-in

**Build tier:** premium (same risk as Task 2, on smart mode, the default mode)

**Files:**
- Modify: `internal/permissions/gate.go` (smart branch of `resolve`)
- Modify: `internal/permissions/patterns.go` (new `unparseableBash`)
- Test: `internal/permissions/unparseable_test.go` (created in Task 2)

- [ ] **Step 1: Write the failing tests.** Append to `internal/permissions/unparseable_test.go`:

```go
// smartUnparseableRun runs one smart-mode gate over a stub registry and
// returns whether the call was denied and every decision it emitted.
func smartUnparseableRun(t *testing.T, autoApprove, alwaysPrompt []string, a Approver, cmd string) (bool, []Decision) {
	t.Helper()
	ctx, rec := recordDecisions()
	cfg := config.PermissionsConfig{Mode: "smart", AutoApprove: autoApprove, AlwaysPrompt: alwaysPrompt}
	g := New(cfg, newTestRegistry("bash"), a, WithWorkspaceRoot(t.TempDir()))
	res := g.Execute(ctx, "bash", bashArgs(cmd))
	return res.Denied, *rec
}

// TestSmartMode_Unparseable_PolicyDenies: in smart mode (the default) a
// policy stand-in never answers a command the parser refuses (change 0096).
// The legacy first-token allow keeps admitting it (ADR-0068's pinned
// boundary), always_prompt keeps its own deny, and a parseable command no rule
// names keeps the headless ask-then-allow posture.
func TestSmartMode_Unparseable_PolicyDenies(t *testing.T) {
	for _, cmd := range []string{"sudo rm -rf /", "/bin/rm -rf /"} {
		t.Run("no rules denies "+cmd, func(t *testing.T) {
			denied, ds := smartUnparseableRun(t, nil, nil, Policy(failIfCalled(t)), cmd)
			if !denied || len(ds) != 1 {
				t.Fatalf("want one deny, got denied=%v %+v", denied, ds)
			}
			d := ds[0]
			if d.Verdict != "deny" || d.Layer != LayerRules || d.DecidedBy != DecidedByPolicy || d.Mode != "smart" ||
				d.Reason != unparseableDenyPrefix+cmd {
				t.Errorf("want deny/rules/policy/smart with the unparseable reason, got %+v", d)
			}
		})
	}
	t.Run("legacy first-token allow still admits it", func(t *testing.T) {
		denied, ds := smartUnparseableRun(t, []string{"bash:sudo"}, nil, Policy(failIfCalled(t)), "sudo ls")
		if denied || len(ds) != 1 || ds[0].Verdict != "allow" || ds[0].Layer != LayerSmartConfig {
			t.Fatalf("want one allow at the smart_config layer, got denied=%v %+v", denied, ds)
		}
	})
	t.Run("always_prompt keeps its explicit-ask deny", func(t *testing.T) {
		denied, ds := smartUnparseableRun(t, nil, []string{"bash:sudo*"}, Policy(failIfCalled(t)), "sudo ls")
		if !denied || len(ds) != 1 || ds[0].Layer != LayerRules || ds[0].DecidedBy != DecidedByPolicy ||
			ds[0].Reason != "matched ask rule bash:sudo*; no approver bound" {
			t.Fatalf("want the explicit-ask policy deny naming bash:sudo*, got denied=%v %+v", denied, ds)
		}
	})
	t.Run("a parseable unmatched command keeps the headless posture", func(t *testing.T) {
		approve, called := newApproveRecorder(true)
		denied, ds := smartUnparseableRun(t, nil, nil, Policy(approve), "make build")
		if denied || !*called || len(ds) != 2 {
			t.Fatalf("want ask then allow by the stand-in, got denied=%v called=%v %+v", denied, *called, ds)
		}
		if ds[0].Verdict != "ask" || ds[0].Layer != LayerHuman || ds[0].Reason != "" {
			t.Errorf("want ask/human with no reason, got %+v", ds[0])
		}
		if ds[1].Verdict != "allow" || ds[1].Layer != LayerHuman || ds[1].DecidedBy != DecidedByPolicy {
			t.Errorf("want allow/human/policy, got %+v", ds[1])
		}
	})
}

// TestSmartMode_Unparseable_HumanAsksAtParseLayer: with a human bound, a
// refused command still prompts, and the smart-mode ask now names the parse
// layer, as auto mode's does (change 0096).
func TestSmartMode_Unparseable_HumanAsksAtParseLayer(t *testing.T) {
	approve, called := newApproveRecorder(true)
	denied, ds := smartUnparseableRun(t, nil, nil, Human(approve), "sudo ls")
	if denied || !*called || len(ds) != 2 {
		t.Fatalf("want ask then the human's allow, got denied=%v called=%v %+v", denied, *called, ds)
	}
	if ds[0].Verdict != "ask" || ds[0].Layer != LayerParse || ds[0].Reason != "" || ds[0].Mode != "smart" {
		t.Errorf("want ask/parse/smart with no reason, got %+v", ds[0])
	}
	if ds[1].Verdict != "allow" || ds[1].Layer != LayerHuman || ds[1].DecidedBy != DecidedByHuman {
		t.Errorf("want allow/human/human, got %+v", ds[1])
	}
}
```

- [ ] **Step 2: Run and confirm failure.**
  `go test ./internal/permissions/ -run 'TestSmartMode_Unparseable_'`. Expect the two "no rules
  denies" subtests (the stand-in approves today, and `failIfCalled` fires) and the human
  parse-layer test (today's ask is at `human`) to fail; the other three subtests pass already.

- [ ] **Step 3: Implement.** In `internal/permissions/patterns.go`, add after
  `catastrophicSegment`:

```go
// unparseableBash reports whether a tool call is a bash command the shell
// parser refuses, and returns the command for the decision reason (change
// 0096). An empty command parses (to no segments), so it is never reported.
func unparseableBash(toolName, args string) (string, bool) {
	if toolName != "bash" {
		return "", false
	}
	command := bashCommand(args)
	if _, err := splitSegments(command); err != nil {
		return command, true
	}
	return "", false
}
```

In `internal/permissions/gate.go`, smart branch of `resolve`, inside `case !demoted:`, after the
`if matchesPermissive(...) || onSafeList(name) { ... return policy, nil }` block and still inside
that case, add:

```go
				// A command the parser refuses (change 0096) has no segment for
				// any layer to judge, so only a human may answer it. With a policy
				// stand-in it is denied at the rules layer; with a human bound it
				// prompts and the ask event names the parse layer, as in auto
				// mode. The legacy allow just above still admits it when the
				// operator named it (auto_approve: ["bash:sudo"]), the boundary
				// change 0093 pinned.
				if command, unparseable := unparseableBash(name, args); unparseable {
					if g.approvalProvenance == DecidedByPolicy {
						reason := unparseableDenyPrefix + command
						g.emitDecision(ctx, mode, name, args, "deny", LayerRules, reason, g.approvalProvenance, nil)
						return ToolPolicy{Enabled: true, AutoApprove: false, DenyReason: reason, DenyLayer: LayerRules}, nil
					}
					askLayer = LayerParse
				}
```

In the catastrophic-floor comment at the top of the smart branch, replace "Commands the parser
refuses keep their prior behavior." with "A command the parser refuses is never catastrophic here;
it is handled after the allow step below (change 0096)."

`auto.deny` and `auto.ask` stay auto-mode rules: do not read them in the smart branch.

- [ ] **Step 4: Verify.** `go test ./internal/permissions/ -run 'TestSmartMode_Unparseable_|TestSmartLegacyAllow_KeepsCatastrophicFloor|TestProtectiveRawSubjects'`
  then `go test ./internal/permissions/`. All pass.

- [ ] **Step 5: Commit.** `fix(permissions): deny unparseable bash under a policy stand-in in smart mode`

---

### Task 4: Protective parity for unparseable commands across smart and auto mode

**Build tier:** standard

**Files:**
- Modify: `internal/permissions/patternparity_test.go`

This is the spec's parity table, kept separate from `protectiveParityRows` (see Deviations, item 1).
Leave `protectiveParityRows` and its two tests exactly as they are.

- [ ] **Step 1: Add the table and tests.** Append to `internal/permissions/patternparity_test.go`:

```go
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
```

- [ ] **Step 2: Verify.** `go test ./internal/permissions/ -run 'TestProtectivePatterns_'` passes
  (both the existing parity tests and the two new ones), then `go test ./internal/permissions/`.
  These tests pin behavior Tasks 1 to 3 built; to confirm they discriminate, temporarily revert the
  `resolveAuto` parse-failure block to `return VerdictAsk, LayerParse, "", nil`, see the new tests
  fail, and restore it before committing.

- [ ] **Step 3: Commit.** `test(permissions): protective parity for unparseable bash in smart and auto mode`

---

### Task 5: Composition-root and end-to-end proof on the hosted bindings

**Build tier:** standard

**Files:**
- Create: `cmd/fuse/permission_unparseable_binding_test.go`

Reuses `floorWorkspace`, `scriptedBashCall`, `scriptedFinal`, `runScriptedLoop` and `modelWasTold`
from `cmd/fuse/permission_floor_binding_test.go`, and `harnessServer` / `harnessOperator` from
`cmd/fuse/loop_harness_test.go`. Do not duplicate them.

- [ ] **Step 1: Write the tests.** Create `cmd/fuse/permission_unparseable_binding_test.go`:

```go
package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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

const unparseableProbeDenyReason = "unparseable command denied; no approver bound: " + unparseableProbe

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
// auto.deny rule naming the raw text denies it with the plain rule reason. The
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
	}{
		{"auto mode", "permissions:\n  mode: auto\n", "auto", unparseableProbeDenyReason, permissions.DecidedByPolicy},
		{"smart mode, the default", "", "smart", unparseableProbeDenyReason, permissions.DecidedByPolicy},
		{"auto deny rule on the raw text", "permissions:\n  mode: auto\n  auto:\n    deny: [\"bash:/usr/bin/*\"]\n",
			"auto", "denied by auto-mode rules layer: " + unparseableProbe, ""},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			floorWorkspace(t)
			rt, gw, _ := harnessServer(t, harnessOperator+tc.yaml)
			gw.script = []string{scriptedBashCall(unparseableProbe), scriptedFinal}
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
				d.DecidedBy != tc.decidedBy || d.Reason != tc.reason {
				t.Fatalf("want deny/rules/%s decided_by %q reason %q, got %+v", tc.mode, tc.decidedBy, tc.reason, d)
			}
			if !modelWasTold(gw, tc.reason) {
				t.Fatalf("the model was not told the deny reason %q", tc.reason)
			}
		})
	}
}
```

If `event.PermissionDecisionPayload` names its fields differently from `Tool`, `Verdict`, `Layer`,
`Mode`, `DecidedBy` and `Reason`, follow the existing usages in
`cmd/fuse/permission_floor_binding_test.go`, which use those exact names.

- [ ] **Step 2: Verify.**
  `go test ./cmd/fuse/ -run 'TestHostedBindingDeniesUnparseableCommand|TestLoopServerDeniesUnparseableCommandEndToEnd'`
  passes. To confirm the tests bite, temporarily undo Task 2's `resolveAuto` block and Task 3's
  smart-branch check by hand, run them, see the auto and smart cases fail (in the hosted test the
  stand-in then approves and `ran` is created, inside the temp workspace only), restore both edits,
  and rerun green. Then `go test ./cmd/fuse/`.

- [ ] **Step 3: Commit.** `test(fuse): prove hosted bindings deny unparseable bash end to end`

---

### Task 6: Documentation

**Build tier:** economy

**Files:**
- Modify: `README.md` (permissions section around lines 259-261; loop-server paragraph around
  lines 479-483)

- [ ] **Step 1: Permissions section.** In `README.md`, replace

```markdown
Commands the parser refuses (`sudo`, path-qualified argv0, `env` with non-inert
assignments) are not matched by wrapper-named protective rules in auto mode,
which asks for them instead. A matching `always_prompt` or
```

with

```markdown
Commands the parser refuses (`sudo`, `npx`, `xargs`, a path-qualified argv0,
`env` with non-inert assignments, `bash` without `-c`) are matched by protective
rules against the command text as written in every mode. With a human approver
bound they prompt; with only a policy stand-in (the loop servers, the headless
mcp-server, the research probe, piped stdin) they are denied at the rules layer
with `unparseable command denied; no approver bound: <command>`, or with the
matching rule's reason when one fires. `auto_approve` never admits them in auto
mode; in smart mode only a first-token `auto_approve` entry such as `bash:sudo`
does. A matching `always_prompt` or
```

- [ ] **Step 2: Loop-server section.** In the paragraph that begins "Neither `loop-server` nor
  `loop-serve-net` has a human approver", after its last sentence ("To let such a command run
  unattended, move its pattern to `auto_approve` or remove it."), append:

```markdown
A command the permission parser refuses (`sudo`, `npx`, `xargs`, a
path-qualified binary, `env` with non-inert assignments) is denied the same way,
with `unparseable command denied; no approver bound: <command>`: a hosted loop
cannot run one unless the loop runs in smart mode with a first-token
`auto_approve` rule naming it, such as `bash:sudo`.
```

- [ ] **Step 3: Sweep for stale claims.** Run
  `grep -rn -i "parser refuses\|parser-refused\|asks for them instead" README.md docs --include='*.md'`
  and fix any remaining sentence that says a policy stand-in approves, or auto mode asks for, a
  command the parser refuses. Do not edit `docs/superpowers/`, `docs/results/` or `docs/reviews/`
  (historical records). Confirm there are no em dashes in the lines you added.

- [ ] **Step 4: Commit.** `docs: unparseable bash is denied when no human approver is bound`

---

## Final task (coordinator, not a build worker): record the ADR

Not a `### Task`: `docket-build` must not dispatch it. After the build gate passes, the coordinator
(`docket-implement-next`) records one ADR through `docket-adr`, linked to change 0096. Build workers
do not create ADR files.

- **Title:** Commands the parser refuses are never answered by a policy stand-in
- **Status:** Accepted. `supersedes: []`. `relates_to:` ADR-0070 (approval provenance is per gate;
  the current explicit-ask ADR, which superseded ADR-0066) and ADR-0068 (catastrophic floor).
- **Context:** the parser refuses commands it cannot decompose honestly (arbitrary-arg wrappers such
  as `sudo`, `npx`, `xargs`; a path-qualified argv0; a non-inert `env` assignment; an opaque command
  word; `bash` without `-c`; process substitution; over-length input). Auto mode asked for them
  before reading any rule, and smart mode's floor could not see them, so under a policy stand-in
  (loop servers, headless mcp-server, research probe, piped stdin) the most dangerous shapes were
  approved while their plain forms were denied.
- **Decision:**
  - A command the parser refuses is never answered by a policy stand-in. In auto and smart mode,
    with only a policy stand-in bound, it is denied at the rules layer with
    `unparseable command denied; no approver bound: <command>`, `decided_by: policy`. With a human
    bound it prompts, with the ask event at the parse layer.
  - Protective rules (`auto.deny`, `auto.ask`, `always_prompt`) match the raw command text of an
    unparseable command in every mode, with the slash-spanning matcher, and produce the same reasons
    as for parsed commands. Permissive rules do not match it in auto mode; in smart mode the legacy
    first-token `auto_approve` subject keeps admitting it, as ADR-0068 pinned.
  - This narrows ADR-0068's carve-out ("commands the parser refuses keep their prior behavior") and
    closes its known residual. ADR-0068's decisions about parsed commands stand, so it is not
    superseded.
- **Alternatives rejected:** extending the parser to accept more forms (out of scope, and each
  accepted form is a new judgment surface); denying at `LayerParse` (the layer names where a verdict
  was decided, and nothing at the parse layer decides; the human-bound ask stays there because
  nothing has been decided yet); honoring `auto.deny` in smart mode for these commands (smart mode
  has never read `auto.*`; widening its rule set is a different change).
- **Consequences:** hosted and headless runs that relied on a stand-in approving `sudo`, `npx`,
  `xargs`, path-qualified binaries, non-inert `env` prefixes or `bash script.sh` now get a deny the
  model can act on; the only unattended rescue is a smart-mode first-token `auto_approve` entry.
  Protective rules naming a refused wrapper start firing in auto mode. Attended runs still prompt;
  smart-mode asks for these commands now report `layer: parse` instead of `layer: human`.
- After recording, update the change record's `adrs:` to include the new ADR alongside the
  relationship to ADR-0070 and ADR-0068 (the record currently lists 66 and 68).

## Release note (for the PR description)

- Hosted loops (both servers), the headless mcp-server, the research probe and piped-stdin one-shot
  runs no longer run commands the permission parser refuses; they are denied with
  `unparseable command denied; no approver bound: <command>`. Smart mode with a first-token
  `auto_approve` entry (`bash:sudo`, `bash:npx`) still admits them; auto mode offers no rescue.
- Protective rules naming a refused wrapper (`bash:sudo*`, `bash:npx *`, `bash:/bin/*`) now fire in
  auto mode.
- Attended runs are unchanged apart from smart-mode asks for refused commands reporting
  `layer: parse` (previously `layer: human`). No config keys change.
- Known residual, out of scope: the deny hint appended for `LayerRules` denials
  (`denyHint` in `internal/permissions/denyhint.go`) describes the command as catastrophic, which
  is inexact for this deny reason.
