# Smart mode honors always_prompt with no human approver bound

Change 0092 — Smart mode honors always_prompt with no human approver bound

Designed against the permissions package as it stands once the explicit-ask verdict and the protective-vs-permissive matcher have both merged. Line numbers below are approximate for that merged state.

## Problem

In auto mode an explicit ask (an `auto.ask` or `always_prompt` match) is now its own verdict: with a human bound it prompts, and under a policy stand-in it is denied at the rules layer with the matched pattern in the reason (`internal/permissions/gate.go`, `resolveAuto`, around 690-705). Smart mode never reaches that code. Its branch in `resolve` (`gate.go` around 565-577) only asks "did `always_prompt` match?" via `matchesProtective` (`patterns.go` around 15-33), and a hit merely skips the allow step. The call then drops into the shared human-approval block (`gate.go` around 580-600), where `g.approve` is whatever the binding wired. Every headless binding wires `permissions.AlwaysApprove` and marks policy provenance: the stdio loop server (`cmd/fuse/loop_server.go` around 55-56), the networked loop server (`cmd/fuse/loop_serve_net.go` around 322-323), the headless mcp-server with no `FUSE_HITL_SOCKET` (`cmd/fuse/mcp_server.go` around 52-61), and the piped-stdin one-shot, whose stand-in denies with an error (`cmd/fuse/approval.go` around 29-33, 66-72). So in smart mode, which is the default (`internal/config/schema.go` around 745; `ParseMode` in `policy.go` around 41-52 maps anything unknown to smart), an `always_prompt` match under a policy stand-in is recorded as `ask` then `allow` with `decided_by: policy`, and the command runs. The same rule denies one mode over.

A second, smaller defect: `matchesProtective` returns a bool, so even a correct smart-mode deny could not name the pattern the way the auto-mode reason does.

`--approve-all` is not one of these cases. It sets `permissions.mode` to `off` for the run (`cmd/fuse/main.go` around 174-176), and mode off returns at `LayerModeOff` before any rule is read (`gate.go` around 522-526). ADR-0064 lists it as a policy stand-in; that wording is wrong and is corrected below.

## Design

One rule for every mode that evaluates operator rules: an explicit ask that no human can answer is a deny.

- `rules.go`: `protectiveHit(patterns, segments) bool` becomes `protectiveMatch(patterns, segments) (pattern string, ok bool)`, returning the first pattern that hits any protective subject (segment, head, whole command). Keep `protectiveHit` as a one-line wrapper for the deny call site. `evalRulesDetail` takes its ask pattern from `protectiveMatch`, so the ask reason is produced by the same slash-spanning matcher as the hit. The `path.Match`-based `firstMatch` helper from the explicit-ask branch is deleted; it must not survive the merge, or an ask could hit via `matchProtective` and then fail to name its pattern.
- `patterns.go`: `matchesProtective(patterns, toolName, args) bool` becomes `protectivePattern(patterns, toolName, args) (string, bool)`, same subject order as today (legacy `{tool, bash:<firstToken>}`, then the parsed segments through `protectiveMatch`, or the raw text when the parser refuses). The tool-name subject means `always_prompt: ["web_fetch"]` names `web_fetch` as its pattern.
- `gate.go` `resolve`, smart branch:
  - `pat, demoted := protectivePattern(g.cfg.AlwaysPrompt, name, args)`.
  - Not demoted: the allow step is unchanged (`matchesPermissive` or `onSafeList`, event `allow` at `LayerSmartConfig`).
  - Demoted and `g.approvalProvenance == DecidedByPolicy`: emit `deny` at `LayerRules`, reason `matched ask rule <pat>; no approver bound`, `decided_by: policy`, and return `ToolPolicy{Enabled: true, AutoApprove: false, DenyReason: reason, DenyLayer: LayerRules}`. This is byte-for-byte the auto-mode outcome, so the decision event and the tool result the model sees are identical in both modes.
  - Demoted with a human bound: set `askLayer = LayerRules` and `askReason = "matched ask rule " + pat`, then fall into the existing approval block. The prompt is unchanged; only the pre-approval `ask` event now says why it appeared, as auto's does.
- The session cache check (`gate.go` around 528-532) stays ahead of the smart branch. A headless gate never populates it (`AlwaysApprove` returns `allowForSession=false`), so nothing changes there.
- Prompt-all mode is untouched: it has no operator rule to honor, and its headless posture (every call asked, every ask policy-answered) is the documented binding choice.
- Docs: the README "How patterns match" paragraph currently says an auto-mode ask from a protective rule "is not yet final" and a later layer can still allow it. That is false once the explicit-ask verdict lands. Replace it with: an `always_prompt` or `auto.ask` match is final in smart and auto alike; it prompts the human, and when the binding has no human (loop servers, headless mcp-server, piped stdin) it is denied with the pattern in the reason. The loop-server section gets one sentence saying the same.
- ADR: record a new ADR, status Accepted, `supersedes: [64]`, and mark ADR-0064 Superseded. Corrected wording for the decision:
  - An explicit ask rule match (`auto.ask` or `always_prompt` in auto mode; `always_prompt` in smart mode) is its own verdict in both modes. It never falls through to the safelist, heuristics, classifier, or smart-mode allow step.
  - With a human approver bound, it prompts that human. With only a policy stand-in bound, it is denied at the rules layer with reason `matched ask rule <pattern>; no approver bound`, `decided_by: policy`.
  - The policy stand-in bindings are: the stdio loop server, the networked loop server, the headless mcp-server (no HITL socket), and the piped-stdin one-shot deny fallback. `--approve-all` is not one of them: it runs the session in mode off, where no rule is evaluated and nothing is asked. ADR-0064 listed it as a stand-in; this ADR corrects that.
  - "No rule matched" is unchanged in both modes.
  - Alternatives: routing the smart-mode ask to the stand-in (the implicit allow this removes); a smart-mode-only reason string (rejected: one rule, one event shape, in every mode); denying at `LayerSmartConfig` (rejected: the rule is an operator rule and the auto-mode twin already reports `rules`).

## Testing

`internal/permissions`:

- `TestSmartMode_AlwaysPrompt_HumanPrompts`: mode smart, `always_prompt: ["bash:cat*"]`, provenance human, `cat x.txt`. The approver is called; the recorded `ask` decision has `Layer == LayerRules` and `Reason == "matched ask rule bash:cat*"`.
- `TestSmartMode_AlwaysPrompt_PolicyStandInDenies`, table over `{"bash:cat*", "cat x.txt"}`, `{"bash:rm *", "rm -rf a/b"}` (slash-spanning), `{"web_fetch", web_fetch args}` (tool-name subject), `{"bash:sudo *", "sudo ls"}` (parser refuses; raw-text subject). Gate built with `AlwaysApprove` and `WithApprovalProvenance(DecidedByPolicy)`, approver wrapped in a recorder that fails the test if called. Assert `res.IsError && res.Denied`, `res.Output` contains `matched ask rule <pat>` and `no approver bound`, and one decision with `Verdict == "deny"`, `Layer == LayerRules`, `DecidedBy == DecidedByPolicy`, `Mode == "smart"`.
- `TestSmartMode_PolicyStandIn_UnmatchedStillAllows`: same gate, no `always_prompt`, `make build`: the stand-in allows (`ask` then `allow` at `LayerHuman`, `decided_by: policy`); and `auto_approve: ["bash:git"]` with `git status` allows at `LayerSmartConfig`. The headless posture for everything that is not an explicit ask is unchanged.
- `TestProtectivePatterns_SameVerdictInSmartAndAuto` gains a policy-provenance pass over `protectiveParityRows`: for every row that hits, smart `always_prompt` and auto `always_prompt` both produce one `deny`/`LayerRules`/`policy` decision with identical `Reason` strings; rows that miss produce no rules-layer deny in either mode.
- `TestProtectiveMatch_NamesTheFirstHit`: `protectiveMatch([]string{"bash:mv *", "bash:rm *"}, segsFor("rm -rf a/b"))` returns `"bash:rm *", true`; `protectivePattern` on the raw JSON args returns the same.
- Existing fixtures (`TestEvalRules_*`, `TestMatchesProtective`, `TestSmartMode_*`) pass unchanged apart from the mechanical rename.

`cmd/fuse`:

- `TestHostedBindingDeniesAlwaysPromptInSmartMode`: mirror of `TestHostedBindingDeniesExplicitAskRule` with `Mode: "smart"` and `AlwaysPrompt: []string{"bash:touch*"}`, built through `buildGate` under `markPolicyApproval`. Assert the deny, the `deny`/`rules` decision naming `bash:touch*`, and that the `ran` sentinel does not exist.
- `TestLoopServerDeniesAlwaysPromptEndToEnd` (end-to-end, production loop-server wiring): `harnessServer(t, harnessOperator + "permissions:\n  always_prompt: [\"bash:touch*\"]\n")`, no `mode` key, so the loop runs in smart mode. `gw.script` is the two-entry script from the explicit-ask test (a `bash` tool call `touch ran`, then a final text reply). `StartLoop`, `Observe` to drain, `Wait`, then `Attach(..., 0)` for the durable history. Assert exactly one `permission.decision` with `Verdict == "deny"`, `Layer == permissions.LayerRules`, `DecidedBy == permissions.DecidedByPolicy`, `Mode == "smart"`, `Reason` containing `bash:touch*`; assert no decision for that tool has `Verdict == "allow"`; and assert `gw.calls[1].Messages` contains a `tool`-role message whose content contains `matched ask rule bash:touch*`, which proves the model was told why. The harness binds no sandbox, so the decision event plus the hosted-binding test above are the proof of non-execution.

## Compatibility

Hosted loops in smart mode (the default) that set `always_prompt` have been running those commands. After this change they are denied with a reason the model can act on. Operators who want them to run unattended move the pattern to `auto_approve` or drop it. The pre-approval `ask` event in smart mode now carries `layer: rules` and a reason when an `always_prompt` rule caused it (previously `layer: human`, empty reason); consumers that keyed on `human` for that case should key on the `ask` verdict. No config key changes. Call the hosted-loop behavior change out in the PR and release note.

## One change or two

Keep this and the floor-and-wrapper-parity change separate, and build this one first. This change introduces the pattern-returning protective matcher and the smart-mode deny path; the other extends the protective subjects and adds a second deny path on the same branch, so stacking it on this one keeps each PR reviewable and each ADR about one decision. Merging them would couple a hosted-behavior change that needs its own release note to three tightenings with their own.
