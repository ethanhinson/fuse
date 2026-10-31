<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0096 — Unparseable shell commands are denied when no human approver is bound](../../changes/archive/2026-10-31-0096-unparseable-shell-commands-are-denied-when-no-human-approver.md)**
<!-- docket:backlink:end -->

# Unparseable shell commands are denied when no human approver is bound

Designed against the permissions package once the floor-and-wrapper-parity change (PR #116) and the per-gate provenance change (0094) have both merged. Line numbers are approximate for that state and are cited from the #116 branch; `g.approvalProvenance` below is the per-gate value 0094 makes it.

## Problem

The shell parser refuses a command it cannot decompose honestly (`splitSegments`, `internal/permissions/shellparse.go` around 459-487): an arbitrary-arg wrapper as argv0 (`arbitraryArgWrappers`, around 193-200: `sudo`, `npx`, `xargs`, `watch`, `eval`, `exec`), a path-qualified argv0 (`/bin/rm`, `./x`, around 938-941), an env-assignment prefix that is not provably inert (`FOO=$(id) make`, around 847-849), an opaque command word (`$CMD x`, around 895-898), `bash`/`sh` without a parseable `-c` (around 949-956), process substitution, arithmetic, a too-long command, or a syntax error. Refusal is the right call: there is no segment to judge. What happens next is not.

Auto mode. `resolveAuto` turns the refusal into `VerdictAsk` at `LayerParse` before any rule is read (`internal/permissions/gate.go` around 737-741). The deny rules, the catastrophic floor and the explicit ask rules all live in `evalRulesDetail`, which needs segments (`internal/permissions/rules.go` around 98-127), so none of them see the command. The ask falls to the human block (`gate.go` around 614-633). With a human bound that is a prompt, which is fine. With a policy stand-in it is `ask` then `allow` at `LayerHuman`, `decided_by: policy`, and the command runs. So in a hosted loop, `sudo rm -rf /`, `/bin/rm -rf /` and `env HOME=$(cat x) rm -rf ~` are approved, while `rm -rf /` is denied by the floor and `rm -rf a/b` under `deny: ["bash:rm *"]` is denied by the rules. The most dangerous shapes are the ones every protective layer skips. The README says as much (`README.md` around 259-261: parser-refused commands "are not matched by wrapper-named protective rules in auto mode, which asks for them instead"), and the floor ADR records it as a known residual.

Smart mode. `catastrophicSegment` returns false on a parse error (`internal/permissions/patterns.go` around 131-134). `protectivePattern` does match `always_prompt` against the raw text when the parser refuses (`patterns.go` around 30-37), so an operator who wrote `always_prompt: ["bash:sudo*"]` is covered. Without such a rule the call reaches the allow step, where the legacy first-token subject may admit it (`matchesPermissive`, `patterns.go` around 70-97; `bash:sudo` in `auto_approve` is the pinned boundary from the floor change), and otherwise falls to the human block, where a stand-in approves it. Headless smart mode (the default mode) therefore runs `sudo rm -rf /` with no rule at all.

Two defects, then: a stand-in answers an ask that only exists because the gate could not judge the command, and in auto mode the operator's protective rules never get to see the raw text that smart mode already matches.

## Design

One rule for both modes: a command the parser refuses is never answered by a policy stand-in, and protective rules see its raw text in every mode.

### Shared helper

- `patterns.go`: `rawCommandSubject(command string) (string, bool)` returns `"bash:" + strings.Join(strings.Fields(command), " ")` and false on an empty command. It is the one raw subject for unparseable commands; `protectivePattern`'s inline `strings.TrimSpace` fallback (around 31-36) is replaced by it so smart and auto normalise identically.
- `rules.go`: `evalUnparseable(command string, cfg AutoConfig, alwaysPrompt []string) (Verdict, string)`: deny patterns first (`firstProtectiveMatch(cfg.Deny, subject)` → `VerdictDeny`), then ask (`cfg.Ask` then `alwaysPrompt`, in that order, → `VerdictAskExplicit` with the pattern), else `VerdictAsk`. No allow: an unparseable command cannot satisfy `allSegmentsAllowed` today and still cannot. The matcher is `matchProtective`, the slash-spanning one, so `bash:/bin/*` reaches `/bin/rm -rf /`.

### Auto mode (`resolveAuto`, `gate.go` around 737-741)

Replace the parse-failure early return with:

- `VerdictDeny` from `evalUnparseable`: return `VerdictDeny, LayerRules, "denied by auto-mode rules layer: " + command`, the same reason the parsed path produces (around 745), so a consumer cannot tell a raw-text deny from a segment deny, which is the point.
- `VerdictAskExplicit`: the existing explicit-ask branch (around 747-760): policy provenance returns `VerdictDeny, LayerRules, explicitAskReason(pat, true)`; human returns `VerdictAsk, LayerRules, explicitAskReason(pat, false)`.
- `VerdictAsk` (no protective hit): human provenance returns `VerdictAsk, LayerParse, ""` exactly as today, so the countable parse ask is unchanged for attended runs. Policy provenance returns `VerdictDeny, LayerRules, unparseableDenyPrefix + command` with
  ```go
  // unparseableDenyPrefix opens the reason when a command the parser refuses
  // reaches a gate with no human to ask (change 0096).
  const unparseableDenyPrefix = "unparseable command denied; no approver bound: "
  ```
  The deny is recorded at `LayerRules` with `decided_by: policy`, through the same `decidedBy` stamping the explicit-ask deny uses in `resolve` (around 553-560); extend that `HasPrefix` check to the new prefix, or better, replace it with a small `policyForcedDeny(reason) bool` helper that knows all three prefixes (explicit ask, catastrophic floor, unparseable).

The order inside `resolveAuto` is otherwise unchanged: the web_fetch floor is before, the safelist, heuristics and classifier after, and none of the after-layers run for an unparseable command, as today.

### Smart mode (`resolve`, smart branch, `gate.go` around 568-610)

- The catastrophic floor and the `always_prompt` check keep their order and behavior; `protectivePattern` already matches the raw subject, now through `rawCommandSubject`.
- Not demoted, allow step unchanged: the legacy first-token subject still admits a parser-refused command when the operator wrote `auto_approve: ["bash:sudo"]`, in every binding. That boundary was pinned by the floor change (`TestSmartLegacyAllow_KeepsCatastrophicFloor`, "parser refusal keeps the legacy allow") and stays: it is the operator's explicit choice, and the one way to run such a command headless in smart mode. Auto mode has no such allow.
- After the allow step misses, add: for a bash call whose `splitSegments` fails, policy provenance emits `deny` at `LayerRules`, reason `unparseableDenyPrefix + command`, `decided_by: policy`, and returns the non-auto-approve policy with that `DenyReason`/`DenyLayer`. Human provenance sets `askLayer = LayerParse` (reason empty) and falls to the prompt, so the smart-mode ask event says why it appeared, as auto's does. Parse the command once per `resolve`; `catastrophicSegment`, `protectivePattern` and `matchesPermissive` each parse today, and a `parsedBash(name, args) (segments, err, ok)` memo on the call avoids a fourth parse. Behavior, not performance, is the goal; do not restructure the three helpers for it.
- `auto.deny` and `auto.ask` stay auto-mode rules. Smart mode's only protective rule is `always_prompt`, as before.

### Prompt-all mode

Untouched. Every call is asked and the stand-in's answer is the documented posture of that mode.

### Docs

- `README.md` around 259-261: replace the sentence with: "Commands the parser refuses (`sudo`, `npx`, `xargs`, a path-qualified argv0, `env` with non-inert assignments, `bash` without `-c`) are matched by protective rules against the command text as written in every mode. With a human approver bound they prompt; with only a policy stand-in (the loop servers, the headless mcp-server, the research probe, piped stdin) they are denied at the rules layer with `unparseable command denied; no approver bound: <command>`, or with the matching rule's reason when one fires. `auto_approve` never admits them in auto mode; in smart mode only a first-token `auto_approve` entry such as `bash:sudo` does."
- The loop-server section gets one sentence: a hosted loop cannot run a command the permission parser refuses unless a smart-mode `auto_approve` first-token rule names it.

### ADR

Record one ADR, status Accepted, `supersedes: []`, relating to the explicit-ask ADR current at build time (0094's successor to ADR-0066) and to the floor ADR (ADR-0068). It extends the explicit-ask rule to commands the parser refuses:

- A command the parser refuses is never answered by a policy stand-in. In auto and smart mode alike, with only a policy stand-in bound, it is denied at the rules layer with `unparseable command denied; no approver bound: <command>`, `decided_by: policy`. With a human bound it prompts, with the ask event at the parse layer.
- Protective rules (`auto.deny`, `auto.ask`, `always_prompt`) match the raw command text of an unparseable command in every mode, with the slash-spanning matcher, and produce the same reasons as for parsed commands. Permissive rules do not match it in auto mode; in smart mode the legacy first-token `auto_approve` subject keeps admitting it, as the floor ADR pinned.
- This narrows the floor ADR's carve-out ("commands the parser refuses keep their prior behavior") and closes its known residual; the floor ADR's decisions about parsed commands stand, so it is not superseded.
- Alternatives: extending the parser to accept more forms (out of scope by the record, and each accepted form is a new judgment surface); denying at `LayerParse` (rejected: the layer names where a verdict was decided, and nothing at the parse layer decides; the human-bound ask stays there because nothing has been decided yet); also honoring `auto.deny` in smart mode for these commands (rejected: smart mode has never read `auto.*`, and widening its rule set is a different change).

## Testing

`internal/permissions`:

- `TestAutoMode_Unparseable_PolicyDenies`, table over `sudo rm -rf /`, `/bin/rm -rf /`, `./rm -rf x`, `FOO=$(id) make`, `npx evil`, `xargs rm`, `$CMD x`, `bash script.sh`, `make <(ls)`, and a 10001-byte command. Gate mode auto, `AlwaysApprove`, no rules, approver wrapped in a recorder that fails the test if called. Each yields `res.IsError && res.Denied`, one `deny`/`LayerRules`/`DecidedByPolicy` decision with `Mode == "auto"`, reason `unparseableDenyPrefix + <command>`, and no `allow` decision.
- `TestAutoMode_Unparseable_HumanStillAsks`: same rows, `Human(recorder)` that approves: decisions are `ask` at `LayerParse` with empty reason, then `allow` at `LayerHuman`, `DecidedByHuman`; the recorder was called once per row. Byte-identical to today's events.
- `TestAutoMode_Unparseable_ProtectiveRulesSeeRawText`, both provenances:
  - `auto.deny: ["bash:sudo*"]`, `sudo ls`: `deny`/`LayerRules`, reason `denied by auto-mode rules layer: sudo ls`, approver not called, `decided_by` empty (a rule deny, not a forced one).
  - `auto.ask: ["bash:/bin/*"]`, `/bin/rm -rf /`: human → `ask`/`LayerRules`/`matched ask rule bash:/bin/*` then the recorder; policy → `deny`/`LayerRules`/`matched ask rule bash:/bin/*; no approver bound`/`DecidedByPolicy`.
  - `always_prompt: ["bash:env *"]`, `env FOO=$(id) make`: same two outcomes naming `bash:env *`.
  - `auto.deny: ["bash:rm *"]`, `sudo rm -rf /`: no hit (the raw subject is `bash:sudo rm -rf /`; a rule naming the inner command does not reach a wrapper the parser refuses); human asks at `LayerParse`, policy denies with the unparseable reason. This pins that raw matching is not a peel.
  - `auto_approve: ["bash:sudo"]`, `sudo ls`, auto mode: not allowed in either provenance (human asks at `LayerParse`, policy denies).
- `TestSmartMode_Unparseable_PolicyDenies`: mode smart, `AlwaysApprove`. `sudo rm -rf /` and `/bin/rm -rf /` with no rules: `deny`/`LayerRules`/`DecidedByPolicy`, reason `unparseableDenyPrefix + <command>`, `Mode == "smart"`, approver not called. With `auto_approve: ["bash:sudo"]`, `sudo ls` allows at `LayerSmartConfig` (pinned boundary). With `always_prompt: ["bash:sudo*"]`, `sudo ls` denies naming `bash:sudo*` (existing path, unchanged). `make build` (parses, no rule) still goes `ask`/`LayerHuman` then `allow`/`LayerHuman`/`DecidedByPolicy`: the headless posture for parseable, unmatched commands is untouched.
- `TestSmartMode_Unparseable_HumanAsksAtParseLayer`: mode smart, `Human(recorder)`, `sudo ls`: `ask` at `LayerParse`, then `allow` at `LayerHuman`/`DecidedByHuman`.
- `protectiveParityRows` (`internal/permissions/patternparity_test.go` around 22) gains unparseable rows: `{"bash:sudo*", "sudo ls"}` hit, `{"bash:/bin/*", "/bin/rm -rf /"}` hit, `{"bash:env *", "env FOO=$(id) make"}` hit, `{"bash:npx *", "npx create-x"}` hit, `{"bash:rm *", "sudo rm -rf /"}` miss. `TestProtectivePatterns_SameMatchInSmartAndAuto` and `TestProtectivePatterns_SameVerdictInSmartAndAuto` run them unchanged, so both modes agree on the hit and on the policy-provenance deny reason. Witness allows for the hitting rows are `bash:sudo`, `bash:/bin/rm`, `bash:env`, `bash:npx` in smart mode only; the auto-mode witness column is "none", since auto has no allow for these.
- `TestRawCommandSubject_Normalises`: `"  sudo   ls\t"` → `bash:sudo ls`; `""` and `"   "` → false; `protectivePattern(["bash:sudo ls"], "bash", args("sudo   ls"))` hits.
- `TestEvalUnparseable_DenyBeatsAsk`: `deny: ["bash:sudo*"]` and `ask: ["bash:sudo*"]` on `sudo ls` yields `VerdictDeny`.
- Existing fixtures (`TestSmartLegacyAllow_KeepsCatastrophicFloor`, `TestProtectiveRawSubjects`, `TestEvalRules_*`, the decision tests on `sudoedit`, which parses) pass unchanged.

`cmd/fuse`:

- `TestHostedBindingDeniesUnparseableCommand`: `buildGate(cfg, toolReg, permissions.AlwaysApprove, nil, nil, nil)` (0094's signature), chdir into a temp dir, execute `bash` with `/usr/bin/touch ran` (path-qualified argv0: refused by the parser, harmless and real on the host). Mode auto and mode smart each assert `res.Denied`, one `deny`/`LayerRules`/`DecidedByPolicy` decision with the unparseable reason, and that `ran` does not exist. A third pass, mode smart with `auto_approve: ["bash:/usr/bin/touch"]`, asserts the file exists and the decision is `allow` at `LayerSmartConfig`: the legacy boundary, pinned at the composition root.
- `TestLoopServerDeniesUnparseableCommandEndToEnd` (end-to-end, production loop-server wiring): `harnessServer(t, harnessOperator + "permissions:\n  mode: auto\n")`; `gw.script` is `scriptedBashCall("/usr/bin/touch ran")` then `scriptedFinal`. `StartLoop`, drain `Observe`, `Wait`, fold in `Attach(..., 0)`. Assert exactly one `permission.decision` with `Verdict == "deny"`, `Layer == permissions.LayerRules`, `DecidedBy == permissions.DecidedByPolicy`, `Mode == "auto"`, `Reason` starting `unparseable command denied; no approver bound: ` and containing `/usr/bin/touch ran`; assert no `allow` decision for that call and no `ask` decision at `LayerParse`; assert `gw.calls[1].Messages` has a `tool`-role message carrying that reason. Second run with `mode` omitted (smart): same assertions with `Mode == "smart"`. Third run, auto with `auto:\n    deny: ["bash:/usr/bin/*"]`: the deny reason is `denied by auto-mode rules layer: /usr/bin/touch ran` and `decided_by` is empty. The harness binds no sandbox; the decision event plus the hosted-binding test above are the proof of non-execution.

## Compatibility

Tightenings in the safe direction, each for the release note:

- Hosted loops (both servers), the headless mcp-server, the research probe and piped-stdin one-shot runs have been running commands the permission parser refuses: `sudo ...`, `npx ...`, `xargs ...`, `/path/to/cmd ...`, `env` with non-inert assignments, `bash script.sh`. They are now denied with a reason the model can act on. Operators who need such a command unattended run the loop in smart mode with a first-token `auto_approve` entry naming the wrapper (`bash:sudo`, `bash:npx`), which is the pre-existing, documented boundary; auto mode offers no rescue, by design. Expect the model to rewrite `npx x` as `npm exec x` or a path-qualified binary as its bare name, both of which the parser accepts and the normal layers judge.
- Protective rules naming a refused wrapper (`bash:sudo*`, `bash:npx *`, `bash:/bin/*`) start firing in auto mode. They were already the operator's intent and already fired in smart mode.
- Attended runs are unchanged: a refused command still prompts, with the same `ask` event in auto mode and a newly reasoned `ask` at `layer: parse` in smart mode (previously `layer: human`, empty reason). Consumers that keyed on `human` for smart-mode asks should key on the `ask` verdict, as the smart-mode `always_prompt` change already asked.
- No config key changes. The parser accepts nothing new; no allow is widened anywhere.

## Dependencies and order

Build after the floor-and-wrapper-parity change (PR #116) merges and stacked on the per-gate provenance change (0094), in that order. This spec's policy deny keys on `g.approvalProvenance`; 0094 makes that value belong to the gate's own approver, so a child built with a stand-in inside an attended process is denied here rather than approved. Without 0094 the deny would still be right for the hosted bindings but wrong for the research probe. The worker-tools change (0095) is unrelated and may land on either side.
