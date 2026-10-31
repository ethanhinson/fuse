<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0092 — Smart mode honors always_prompt with no human approver bound](../../changes/active/0092-smart-mode-honors-always-prompt-with-no-human-approver-bound.md)**
<!-- docket:backlink:end -->

# Smart mode honors always_prompt with no human approver bound - Implementation Plan

> Executed by `docket-build`: one task at a time, each task TDD (failing test, minimal code, passing test, commit). Every test is in-process: stub tools, the recording `DecisionSink`, and the scripted `httptest` gateway in `cmd/fuse/loop_harness_test.go`. No live model traffic.

**Goal:** In smart mode, an `always_prompt` match is its own verdict exactly as it is in auto mode. With a human approver bound it prompts (and the pre-approval `ask` event now names the rule). With only a policy stand-in bound (loop servers, headless mcp-server, piped stdin) it is denied at `LayerRules` with reason `matched ask rule <pattern>; no approver bound` and `decided_by: policy`, byte-for-byte the auto-mode outcome. Nothing else in smart mode changes.

**Architecture:**
- `internal/permissions/patterns.go`: `matchesProtective(patterns, toolName, args) bool` becomes `protectivePattern(patterns, toolName, args) (string, bool)`. Same subject order as today: legacy `{tool, bash:<firstToken>}` subjects, then parsed segments through the shared protective matcher, or the raw `bash:<text>` when the parser refuses.
- `internal/permissions/rules.go`: the pattern-returning protective matcher the spec calls `protectiveMatch` already exists on this base as `protectiveHitPattern` (with `protectiveHit` as its bool wrapper), and `evalRulesDetail` already takes its ask pattern from it. The `path.Match`-based `firstMatch` helper the spec says to delete is already absent. Reconcile re-map: keep the existing names, do not rename. Task 1 verifies `firstMatch` is gone.
- `internal/permissions/gate.go` `resolve`, smart branch: demoted plus `DecidedByPolicy` returns the rules-layer deny; demoted plus a human sets `askLayer = LayerRules` and `askReason` before the shared approval block. One reason helper is shared with `resolveAuto` so the two modes cannot drift.
- `README.md`: the "How patterns match" paragraph and the loop-server section state the one rule for smart and auto alike.

**Tech stack:** Go, `internal/permissions`, `cmd/fuse`.

**Learnings applied:**
- `security-knob-inert-at-composition-root`: the package tests construct the gate by hand. Task 3 proves the deny through `buildGate` under `markPolicyApproval` and through the production loop-server runtime (`harnessServer`), so the fix is not inert in the binary.
- `canonicalize-once-before-every-matching-layer`: the smart-mode deny reason and the auto-mode deny reason must come from one helper and one matcher (`protectiveHitPattern` / `firstProtectiveMatch`). Do not add a second matcher or a second reason format.
- `no-human-allow-path-admission-is-an-allowlist`: the policy branch keys on `g.approvalProvenance == DecidedByPolicy` exactly as `resolveAuto` does. Do not key on the approve func's identity.
- `scripted-gateway-double-double-escapes-tool-args`: build the e2e tool-call `arguments` with `json.Marshal` of a Go map, then `json.Marshal` that string once, exactly as `TestLoopServerDeniesExplicitAskRuleEndToEnd` does. Never hand-write pre-escaped JSON.

**Decisions made in this plan (record them in the PR body; the ADR superseding ADR-0064 is handled outside this plan):**
1. Keep `protectiveHitPattern` as the spec's `protectiveMatch`; no rename (it is already the single pattern-returning matcher and `protectiveHit` is already its wrapper).
2. `matchesProtective` is replaced, not wrapped: its only callers are `gate.go` and two test files, updated mechanically.
3. Add `explicitAskReason(pat string, policy bool) string` in `gate.go` next to `explicitAskDenyPrefix`, used by both `resolveAuto` and the smart branch, so the reason strings are identical by construction.
4. The smart-mode policy deny passes `g.approvalProvenance` (`policy`) as `decided_by`, matching what the auto deny path emits.
5. Prompt-all mode and the session-cache check are untouched.

**PR body must call out (Compatibility):** hosted loops in smart mode (the default) that set `always_prompt` previously ran matching commands; they are now denied with a reason the model can act on (move the pattern to `auto_approve` or drop it to run unattended). The smart-mode pre-approval `ask` event caused by an `always_prompt` rule now carries `layer: rules` and a reason (previously `layer: human`, empty reason). No config key changes.

---

## Task 1: Pattern-returning smart-mode protective matcher

**Tier:** economy

**Files:**
- Modify: `internal/permissions/patterns.go`
- Modify: `internal/permissions/patterns_test.go`
- Modify: `internal/permissions/patternparity_test.go`
- Modify: `internal/permissions/gate.go` (one call site)

**Step 1: Write the failing test** in `internal/permissions/patterns_test.go`:
```go
// TestProtectiveMatch_NamesTheFirstHit: the protective matcher returns the
// first pattern that hits, through both the parsed-segment and raw-args entry
// points (change 0092), so a deny reason can name it.
func TestProtectiveMatch_NamesTheFirstHit(t *testing.T) {
	pats := []string{"bash:mv *", "bash:rm *"}
	if pat, ok := protectiveHitPattern(pats, segsFor(t, "rm -rf a/b")); !ok || pat != "bash:rm *" {
		t.Fatalf("protectiveHitPattern = %q %v, want bash:rm * true", pat, ok)
	}
	if pat, ok := protectivePattern(pats, "bash", bashArgs("rm -rf a/b")); !ok || pat != "bash:rm *" {
		t.Fatalf("protectivePattern = %q %v, want bash:rm * true", pat, ok)
	}
	if pat, ok := protectivePattern([]string{"web_fetch"}, "web_fetch", `{"url":"https://example.com"}`); !ok || pat != "web_fetch" {
		t.Fatalf("tool-name subject = %q %v, want web_fetch true", pat, ok)
	}
	if pat, ok := protectivePattern([]string{"bash:/usr/bin/rm *"}, "bash", bashArgs("/usr/bin/rm -rf a/b")); !ok || pat != "bash:/usr/bin/rm *" {
		t.Fatalf("raw-text subject = %q %v, want bash:/usr/bin/rm * true", pat, ok)
	}
	if pat, ok := protectivePattern(pats, "bash", bashArgs("ls")); ok || pat != "" {
		t.Fatalf("miss = %q %v, want \"\" false", pat, ok)
	}
}
```

**Step 2: Run, confirm red:** `go test ./internal/permissions -run TestProtectiveMatch_NamesTheFirstHit -count=1` (compile failure on `protectivePattern` is the expected red).

**Step 3: Implement** in `internal/permissions/patterns.go`: replace `matchesProtective` with
```go
// protectivePattern returns the first protective pattern (always_prompt in
// smart mode) that matches a tool call, and whether one did (changes 0088,
// 0092). Subject order is unchanged: the legacy {tool, "bash:<firstToken>"}
// subjects first, so no rule that demoted a call before stops demoting it,
// then the one shared protective subject rule (protectiveHitPattern: each
// parsed segment, its head, and the whole command) with the slash-spanning
// glob. A command the parser refuses is matched on its raw text instead, so a
// protective rule still reaches it. Returning the pattern lets a smart-mode
// deny name the rule exactly as the auto-mode reason does.
func protectivePattern(patterns []string, toolName, args string) (string, bool) {
	if len(patterns) == 0 {
		return "", false
	}
	for _, subj := range subjects(toolName, args) {
		if pat, ok := firstProtectiveMatch(patterns, subj); ok {
			return pat, true
		}
	}
	if toolName != "bash" {
		return "", false
	}
	command := bashCommand(args)
	segments, err := splitSegments(command)
	if err != nil {
		raw := strings.TrimSpace(command)
		if raw == "" {
			return "", false
		}
		return firstProtectiveMatch(patterns, "bash:"+raw)
	}
	return protectiveHitPattern(patterns, segments)
}
```
(`firstProtectiveMatch` in `rules.go` applies `matchProtective` per pattern, which is exactly what `matchesSubjects(consumerProtective, ...)` did, so behavior is unchanged.)

Mechanical call-site renames:
- `gate.go` smart branch (around line 569): `if !matchesProtective(g.cfg.AlwaysPrompt, name, args) {` becomes `if _, demoted := protectivePattern(g.cfg.AlwaysPrompt, name, args); !demoted {`. No behavior change in this task; Task 2 rewrites the branch.
- `patterns_test.go` `TestMatchesProtective`: `if got := matchesProtective(...); got != tc.want` becomes `if _, got := protectivePattern(...); got != tc.want`, with the error message renamed. Keep the test name and every row.
- `patternparity_test.go` `TestProtectivePatterns_SameMatchInSmartAndAuto`: `smart := matchesProtective(...)` becomes `_, smart := protectivePattern(...)`. Update the comment above it (`smart's matchesProtective` becomes `smart's protectivePattern`).
- Update the `protectiveHit` doc comment in `rules.go` (it names `matchesProtective` as a caller) to name `protectivePattern`.

Then verify: `grep -rn "matchesProtective\|firstMatch\b" internal/ cmd/` returns nothing (the spec's `firstMatch` helper must not exist; it does not on this base).

**Step 4: Run** `go test ./internal/permissions -count=1`. All pass.

**Step 5: Commit** `refactor(permissions): return the matched pattern from the smart-mode protective matcher`

---

## Task 2: Smart-mode always_prompt is its own verdict

**Tier:** standard

**Files:**
- Modify: `internal/permissions/gate.go`
- Modify: `internal/permissions/gate_test.go`
- Modify: `internal/permissions/patternparity_test.go`

**Step 1: Write failing tests** in `internal/permissions/gate_test.go` (helpers: `newApproveRecorder`, `recordDecisions`, `newTestRegistry`, `bashArgs` already exist in the package's tests):
- `TestSmartMode_AlwaysPrompt_HumanPrompts`: `cfg := config.PermissionsConfig{Mode: "smart", AlwaysPrompt: []string{"bash:cat*"}}`, gate `New(cfg, newTestRegistry("bash"), approve, WithApprovalProvenance(DecidedByHuman))` with `approve, called := newApproveRecorder(true)`, ctx from `recordDecisions()`, `g.Execute(ctx, "bash", bashArgs("cat x.txt"))`. Assert `*called`. Find the decision with `Verdict == "ask"`; assert `Layer == LayerRules` and `Reason == "matched ask rule bash:cat*"`. Also assert the subsequent human-layer `allow` decision is still `Layer == LayerHuman`, `DecidedBy == DecidedByHuman` (the human outcome event is unchanged).
- `TestSmartMode_AlwaysPrompt_PolicyStandInDenies`: table
  ```go
  {"bash:cat*", "bash", bashArgs("cat x.txt")},
  {"bash:rm *", "bash", bashArgs("rm -rf a/b")},          // slash-spanning
  {"web_fetch", "web_fetch", `{"url":"https://example.com"}`}, // tool-name subject
  {"bash:sudo *", "bash", bashArgs("sudo ls")},           // parser refuses; raw-text subject
  ```
  Before relying on the `sudo ls` row, confirm `splitSegments("sudo ls")` returns an error (`sudo` is in the wrapper table at `shellparse.go` around line 196 and should fail closed). If it parses, replace that row with `{"bash:/usr/bin/rm *", "bash", bashArgs("/usr/bin/rm -rf a/b")}`, the existing unparseable fixture from `TestMatchesProtective`, and note it in the commit message. Per row: registry `newTestRegistry("bash", "web_fetch")`, `cfg := config.PermissionsConfig{Mode: "smart", AlwaysPrompt: []string{row.pat}}`, approver is a func that calls `t.Errorf("approver must not be called under a policy stand-in")` and returns `true, false, nil` (so a regression is both reported and would otherwise allow), gate built with `WithApprovalProvenance(DecidedByPolicy)`. Assert `res.IsError && res.Denied`; `res.Output` contains `"matched ask rule "+row.pat` and `"no approver bound"`; exactly one decision recorded, with `Verdict == "deny"`, `Layer == LayerRules`, `DecidedBy == DecidedByPolicy`, `Mode == "smart"`, `Reason == "matched ask rule "+row.pat+"; no approver bound"`.
- `TestSmartMode_PolicyStandIn_UnmatchedStillAllows`: (a) smart, no `always_prompt`, `AlwaysApprove`, `WithApprovalProvenance(DecidedByPolicy)`, `make build`: not denied; decisions are `ask` at `LayerHuman` then `allow` at `LayerHuman` with `DecidedBy == DecidedByPolicy`. (b) smart, `AutoApprove: []string{"bash:git"}`, `git status`: one `allow` at `LayerSmartConfig`, approver not consulted.

In `internal/permissions/patternparity_test.go`, extend `TestProtectivePatterns_SameVerdictInSmartAndAuto` with a policy-provenance pass after the existing assertions (inside the same `t.Run`):
```go
// Policy pass (change 0092): with only a stand-in bound, a hit is the same
// rules-layer deny in both modes, with identical reasons; a miss is never a
// rules-layer deny.
policyRun := func(cfg config.PermissionsConfig) []Decision {
	ctx, rec := recordDecisions()
	g := New(cfg, newTestRegistry("bash"), AlwaysApprove,
		WithWorkspaceRoot(t.TempDir()), WithApprovalProvenance(DecidedByPolicy))
	g.Execute(ctx, "bash", bashArgs(r.cmd))
	return *rec
}
rulesDenies := func(ds []Decision) []Decision { /* filter Verdict=="deny" && Layer==LayerRules */ }
smartD := rulesDenies(policyRun(smartCfg([]string{r.pattern})))
autoD := rulesDenies(policyRun(autoCfg(config.AutoConfig{}, r.witness, []string{r.pattern})))
```
When `r.hit`: both have exactly one entry, each `DecidedBy == DecidedByPolicy`, and `smartD[0].Reason == autoD[0].Reason`. When `!r.hit`: both are empty. Update the test's doc comment to mention the policy pass.

**Step 2: Run, confirm red:** `go test ./internal/permissions -run 'TestSmartMode_AlwaysPrompt|TestSmartMode_PolicyStandIn|TestProtectivePatterns_SameVerdictInSmartAndAuto' -count=1`. Expected: the policy-denies and parity policy pass fail (smart allows via the stand-in); the human-prompts test fails on `Layer`/`Reason` of the `ask` event. `UnmatchedStillAllows` should already pass (it pins unchanged behavior).

**Step 3: Implement** in `internal/permissions/gate.go`:
- Next to `explicitAskDenyPrefix`, add:
  ```go
  // explicitAskReason renders the reason for an explicit-ask outcome in every
  // mode that evaluates operator rules (auto: auto.ask and always_prompt; smart:
  // always_prompt). policy is true when only a stand-in answers approvals, so
  // the ask is a deny. One helper keeps the two modes' reasons identical.
  func explicitAskReason(pat string, policy bool) string {
  	if policy {
  		return explicitAskDenyPrefix + pat + "; no approver bound"
  	}
  	return explicitAskDenyPrefix + pat
  }
  ```
- In `resolveAuto` `case VerdictAskExplicit`, replace the two string concatenations with `explicitAskReason(askPattern, true)` and `explicitAskReason(askPattern, false)`. Update that comment's binding list only if needed; it already excludes `--approve-all`.
- Replace the smart branch with:
  ```go
  if mode == ModeSmart {
  	// always_prompt is an explicit ask (change 0092): it demotes even
  	// safe-list entries, and like auto mode's it is its own verdict. With a
  	// human bound it prompts and the ask event names the rule; with only a
  	// policy stand-in it is denied at the rules layer, byte-for-byte the
  	// auto-mode outcome, instead of being answered by the stand-in.
  	pat, demoted := protectivePattern(g.cfg.AlwaysPrompt, name, args)
  	switch {
  	case !demoted:
  		// auto_approve config patterns promote beyond the safe list.
  		if matchesPermissive(g.cfg.AutoApprove, name, args, g.workspaceRoot) || onSafeList(name) {
  			g.emitDecision(ctx, mode, name, args, "allow", LayerSmartConfig, "", "", nil)
  			policy.AutoApprove = true
  			return policy, nil
  		}
  	case g.approvalProvenance == DecidedByPolicy:
  		reason := explicitAskReason(pat, true)
  		g.emitDecision(ctx, mode, name, args, "deny", LayerRules, reason, g.approvalProvenance, nil)
  		return ToolPolicy{Enabled: true, AutoApprove: false, DenyReason: reason, DenyLayer: LayerRules}, nil
  	default:
  		askLayer = LayerRules
  		askReason = explicitAskReason(pat, false)
  	}
  }
  ```
- Update the `askLayer`/`askReason` comment block above (it says askReason is set only by the web_fetch floor) to also name the smart-mode `always_prompt` ask.

Do not touch the session-cache check, prompt-all mode, or the shared approval block.

**Step 4: Run** `go test ./internal/permissions -count=1 -race`. All pass, including the unchanged `TestSmartMode_SlashBearingAlwaysPromptDemotes`, `TestAlwaysPromptDemotesSafeList`, `TestBlackboardWriteDemotableViaAlwaysPrompt`, `TestAutoMode_ExplicitAsk_*` and `TestEvalRules_*` (these build gates with the default human provenance, so they still prompt).

**Step 5: Commit** `fix(permissions): deny smart-mode always_prompt matches when no human approver is bound`

---

## Task 3: Composition-root and hosted-loop proofs

**Tier:** standard

**Files:**
- Modify: `cmd/fuse/explicit_ask_binding_test.go`

These are regression proofs at the composition root; with Task 2 landed they are green on arrival. **Red check (required, leaves no diff):** after writing them, temporarily change `case g.approvalProvenance == DecidedByPolicy:` in `gate.go` to `case false && g.approvalProvenance == DecidedByPolicy:`, run both tests and confirm both fail, then restore the line and confirm `git diff internal/permissions/gate.go` is empty before committing.

**Step 1: Write the tests** in `cmd/fuse/explicit_ask_binding_test.go`:
- `TestHostedBindingDeniesAlwaysPromptInSmartMode`: mirror of `TestHostedBindingDeniesExplicitAskRule` (save and restore `gateApprovalProvenance`, `markPolicyApproval()`, host-handler `sandbox.NewService`, `defaultToolRegistry`, `buildGate(cfg, toolReg, permissions.AlwaysApprove, nil, nil, nil)`, `t.Chdir(t.TempDir())`) with `cfg := config.Config{Permissions: config.PermissionsConfig{Mode: "smart", AlwaysPrompt: []string{"bash:touch*"}}}`. Execute `{"command":"touch ran"}`. Assert `res.IsError && res.Denied`; the `ran` sentinel does not exist; a decision with `Verdict == "deny"`, `Layer == permissions.LayerRules`, `DecidedBy == permissions.DecidedByPolicy`, `Mode == "smart"`, `Reason` containing `bash:touch*`; and no decision with `Verdict == "allow"`.
- `TestLoopServerDeniesAlwaysPromptEndToEnd`: production loop-server wiring. Save/restore `gateApprovalProvenance`, `markPolicyApproval()`. Build `toolCall`/`final` exactly as `TestLoopServerDeniesExplicitAskRuleEndToEnd` does (`json.Marshal(map[string]string{"command": "touch ran"})`, then `json.Marshal(string(args))`, embedded once). `rt, gw, _ := harnessServer(t, harnessOperator+"permissions:\n  always_prompt: [\"bash:touch*\"]\n")`, with **no `mode` key**, so the loop runs in the default smart mode. `gw.script = []string{toolCall, final}`. `StartLoop(ctx, runtime.LoopConfig{Task: "go"})`, `Observe` and drain, `h.Wait()`, then `hist, err := rt.Attach(ctx, event.DefaultTenant, h.ID(), 0)`. Count decisions over `hist` only (the durable history is authoritative; the live tail can overlap it, so counting live plus history would double-count). Unmarshal each `event.KindPermissionDecision` payload into `event.PermissionDecisionPayload` and assert: exactly one with `Verdict == "deny"`, and it has `Layer == permissions.LayerRules`, `DecidedBy == permissions.DecidedByPolicy`, `Mode == "smart"`, `Reason` containing `bash:touch*`; no decision with `Tool == "bash"` and `Verdict == "allow"`. Then `calls := gw.snapshot()`; require `len(calls) >= 2`; assert `calls[1].Messages` contains a message with `Role == "tool"` whose `Content` contains `matched ask rule bash:touch*` (the model was told why). Add a doc comment noting the harness binds no sandbox, so the decision event plus `TestHostedBindingDeniesAlwaysPromptInSmartMode` are the proof of non-execution.

If `Mode` on the e2e decision is not `"smart"`, stop and report: it means the hosted default is not smart and the spec's premise needs re-checking. Do not change the assertion to make it pass.

**Step 2: Red check** as described above.

**Step 3: Run** `go test ./cmd/fuse -run 'TestHostedBindingDenies|TestLoopServerDenies' -count=1 -race`. All four (two existing, two new) pass.

**Step 4: Commit** `test(fuse): prove smart-mode always_prompt denies under the hosted bindings`

---

## Task 4: README docs

**Tier:** economy

**Files:**
- Modify: `README.md`

**Step 1: Edit** the "How patterns match (every mode alike)" paragraph (around lines 249-268). Replace the sentence starting `In auto mode a matching \`ask\` or \`always_prompt\` rule is final:` through `classifier) can override it.` with:

> A matching `always_prompt` or `auto.ask` rule is final in smart and auto mode alike: it prompts a human when one is bound, and when the binding has no human (the loop servers, the headless mcp-server, piped stdin) it is denied with the matched pattern in the reason (`matched ask rule <pattern>; no approver bound`). No later layer (the read-only safelist, smart mode's allow step, heuristics or classifier) can override it.

Leave the rest of the paragraph intact.

In the `## Networked loop-control` section, after the first paragraph (the one ending `gRPC-Web (served over h2c):`, around line 460, before the code block), add one paragraph:

> Neither `loop-server` nor `loop-serve-net` has a human approver, so an operator `always_prompt` or `auto.ask` rule cannot prompt anyone: a matching call is denied with the pattern in the reason, in smart mode (the default) and auto mode alike, and the model sees that reason as the tool result. To let such a command run unattended, move its pattern to `auto_approve` or remove it.

Writing rules: no em dashes in new text; no mention of anything outside this repository.

**Step 2: Verify** `grep -n "not yet final" README.md` returns nothing and the two edits render (no broken backticks).

**Step 3: Commit** `docs: always_prompt is final in smart mode and denied with no human approver`

---

## Task 5: Final verification

**Tier:** economy

**Steps:**
1. `gofmt -l internal/permissions cmd/fuse` prints nothing.
2. `go vet ./internal/permissions ./cmd/fuse`.
3. `grep -rn "matchesProtective\|firstMatch\b" --include='*.go' .` prints nothing.
4. `go test ./... -count=1` passes. If a full-suite failure appears in a package this change did not touch, re-run it on the branch tip in a clean detached worktree before attributing it (learning `shared-worktree-red-suite-verify-detached`).

No commit unless a step required a fix.

---

## Acceptance coverage map

| Spec item | Where |
|---|---|
| `protectivePattern` returns the matched pattern, same subject order | Task 1 (`TestProtectiveMatch_NamesTheFirstHit`, `TestMatchesProtective` renamed) |
| `protectiveMatch` / `firstMatch` deletion | Task 1 (re-mapped to existing `protectiveHitPattern`; grep check) |
| Smart + policy: deny at `LayerRules`, pattern in reason, `decided_by: policy` | Task 2 (`TestSmartMode_AlwaysPrompt_PolicyStandInDenies`) |
| Smart + human: prompts, `ask` event names rule at `LayerRules` | Task 2 (`TestSmartMode_AlwaysPrompt_HumanPrompts`) |
| Unmatched smart headless posture unchanged | Task 2 (`TestSmartMode_PolicyStandIn_UnmatchedStillAllows`) |
| Identical reasons in smart and auto | Task 2 (shared `explicitAskReason`; parity policy pass) |
| Composition root: `buildGate` under `markPolicyApproval` | Task 3 (`TestHostedBindingDeniesAlwaysPromptInSmartMode`) |
| End-to-end hosted loop in default smart mode | Task 3 (`TestLoopServerDeniesAlwaysPromptEndToEnd`) |
| README patterns paragraph and loop-server sentence | Task 4 |
| New ADR superseding ADR-0064 | Out of this plan (coordinator, via docket-adr) |
