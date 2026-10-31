<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0093 — Permission floor and wrapper parity across modes and bindings](../../changes/active/0093-permission-floor-and-wrapper-parity-across-modes-and-binding.md)**
<!-- docket:backlink:end -->

# Permission floor and wrapper parity across modes and bindings

Designed against the permissions package once the explicit-ask verdict, the protective-vs-permissive matcher, and the smart-mode always_prompt change have all merged; it builds on the `protectiveMatch` / `protectivePattern` helpers that last change introduces. Line numbers are approximate for that state.

## Problem

Three gaps remain between what the catastrophic floor and the protective matcher do in auto mode and what they do in smart mode or in a headless binding.

1. The floor misses smart mode's legacy allow. `matchesPermissive` (`internal/permissions/patterns.go` around 47-70) first matches the legacy subjects `{tool, bash:<firstToken>}` and returns true on a hit; only the per-segment allow path that follows checks `isDangerous` (`rules.go` around 172-186). So `auto_approve: ["bash:rm"]` approves `rm -rf ~` at `LayerSmartConfig` without a prompt. Worse, in a headless smart gate the floor is absent even with no allow rule: `rm -rf /` finds no allow, drops to the human block (`gate.go` around 580-600), and the policy stand-in approves it. Auto mode denies the same segment terminally before any rule is read (`evalRulesDetail`, `rules.go` around 95-105).

2. Wrapper-named protective rules do not fire in auto mode. The parser peels `timeout`, `nice`, `stdbuf`, `nohup`, `time`, `env` and `bash -c` (`shellparse.go` around 893-930) before building a `Segment`, whose `Name` is the inner command. The protective subjects (`matchesSegment` around 570-575, `protectiveHit` around 582-592) are built from `Name` and `Args`, so `deny: ["bash:timeout*"]` or `ask: ["bash:nice *"]` never see the wrapper. Smart mode's legacy `bash:<firstToken>` subject (`patterns.go` `subjects` around 73-82) sees the raw first word, so `bash:timeout*` fires in smart but not in auto; `bash:nice *` fires in neither, because the head subject has no argument text. `Segment.Raw` already carries the pre-peel source text (`shellparse.go` `rawSegment` around 1521) and nothing outside tests reads it.

3. The headless mcp-server builds its gate with `permissions.New(cfg.Permissions, toolReg, approve, WithApprovalProvenance(...))` (`cmd/fuse/mcp_server.go` around 59-61), with no `WithWorkspaceRoot` and no `WithWriteRoots`. `isCatastrophicRm` (`rules.go` around 194-240) can only recognise `/` and `$HOME` with an empty root, so `rm -rf ..` and `rm -rf ../..` run from inside the workspace are not caught, and the path-scoping heuristic has no roots to scope against. Every other binding goes through `buildGate` (`cmd/fuse/run.go` around 415-438), which binds both.

## Design

### 1. The floor is a rules-layer decision in every mode

- `patterns.go`: `matchesPermissive` parses first. For a bash call whose `splitSegments` succeeds, if any segment `isDangerous(seg, workspaceRoot)` it returns false before the legacy subjects are consulted. A parse that fails (`ErrUnparseable`: `sudo`, `npx`, `xargs`, a path-qualified argv0, an opaque command word) keeps today's legacy-subject match, because there is no segment to judge and the legacy subject only ever saw the first word; `bash:sudo` in `auto_approve` is the operator's explicit choice and stays theirs. This is the tightening the change record names.
- `gate.go` `resolve`, smart branch, before the `always_prompt` check: for a bash call that parses, if any segment is catastrophic:
  - provenance policy: emit `deny` at `LayerRules`, reason `catastrophic command denied by rules floor: <command>`, `decided_by: policy`, and return the non-auto-approve policy with that `DenyReason` and `DenyLayer`. A stand-in never approves a catastrophic command.
  - provenance human: skip the allow step entirely (no rule may approve it) and fall to the prompt with `askLayer = LayerRules`, `askReason = "catastrophic command: <command>"`. The human keeps the last word in interactive smart mode, as today.
  Implement this as one helper `catastrophicSegment(name, args, workspaceRoot) (command string, ok bool)` shared by `matchesPermissive` and the smart branch, so the two cannot disagree. Auto mode's existing floor and its reason string are untouched.

### 2. Raw subjects are protective subjects

- `rules.go` `matchesSegment`, protective consumer only: after the segment subject and the head, also test `bash:<rawHead>` and `bash:<rawText>`, where `rawText = strings.Join(strings.Fields(seg.Raw), " ")` and `rawHead` is its first field. Skip both when `Raw` is empty or `rawText` equals the segment subject's text (nothing was peeled). The permissive consumer never sees them: a bare `bash:timeout` allow must not approve `timeout 5 rm -rf a/b`, and `bash:timeout *` as an allow keeps matching only what the strict per-segment rule admits.
- `shellparse.go`: a segment produced from a peeled `bash -c` / `sh -c` script carries the outer call's raw text as `Raw` (set after `collectStmts` returns, around 958), so `deny: ["bash:bash *"]` reaches `bash -c "rm x"` in both modes. Inner segments keep their own `Name` and `Args`.
- Because `protectiveMatch` is the one subject rule both modes call, smart mode gains the same raw subjects; its legacy `bash:<firstToken>` subject is now redundant with `rawHead` for the first segment but stays, as the previous change promised.
- The README matching paragraph adds one sentence: protective patterns also match the command as written, before wrappers such as `timeout`, `nice`, `env` and `bash -c` are peeled, so a rule naming the wrapper fires in every mode.

### 3. The mcp-server gate is scoped like every other gate

- `cmd/fuse/mcp_server.go`: pass `permissions.WithWorkspaceRoot(workspaceRoot())` and `permissions.WithWriteRoots(gateWriteRoots(cfg))` to `permissions.New`. The binding has no model registry, so it does not go through `buildGate` and gets no classifier; that is unchanged and out of scope. The config-reload path rebuilds tools in place and never rebuilds the gate, so the root is bound once for the process, which is correct: the server's cwd does not move.

### ADR

Record one ADR, Accepted, relating to ADR-0005, ADR-0049 and ADR-0065: the catastrophic floor is a rules-layer decision in every mode and binding, a policy stand-in never approves a catastrophic command, and protective patterns match the command as written as well as as parsed, while permissive patterns match only the parsed, strict form.

## Testing

`internal/permissions`:

- `TestSmartLegacyAllow_KeepsCatastrophicFloor`: mode smart, `auto_approve: ["bash:rm"]`, workspace root bound to a temp dir the test chdirs into, human provenance. `rm -rf ~`, `rm -rf /`, `rm -rf ..` each prompt with an `ask` decision at `LayerRules`; `rm x` allows at `LayerSmartConfig` without a prompt. With `auto_approve: ["bash:mkfs.ext4"]`, `mkfs.ext4 /dev/sda1` prompts. With `auto_approve: ["bash:sudo"]`, `sudo ls` still allows (parser refusal keeps the legacy allow; pinned as the documented boundary).
- `TestSmartMode_PolicyStandIn_DeniesCatastrophic`: same commands, `AlwaysApprove` with `WithApprovalProvenance(DecidedByPolicy)`, with and without `auto_approve: ["bash:rm"]`. Each catastrophic command yields `res.Denied`, one `deny`/`LayerRules`/`policy` decision whose reason starts with `catastrophic command denied by rules floor:`, and the approver is never called. `rm x` with the allow rule allows at `LayerSmartConfig`; without it, the stand-in allows at `LayerHuman` as before.
- `TestProtectiveRawSubjects` extends `protectiveParityRows` with: `{"bash:timeout*", "timeout 5 rm x"}` hit, `{"bash:nice *", "nice -n 5 make"}` hit, `{"bash:env *", "env FOO=1 make"}` hit, `{"bash:bash *", "bash -c \"ls\""}` hit, `{"bash:timeout*", "git status && timeout 5 rm x"}` hit (second segment), `{"bash:rm *", "timeout 5 rm x"}` hit (peeled subject still matches). `TestProtectivePatterns_SameMatchInSmartAndAuto` and `TestProtectivePatterns_SameVerdictInSmartAndAuto` run the new rows unchanged, so both modes must agree. Witness allows for these rows name the peeled segment (`bash:rm x`, `bash:make`, `bash:ls`).
- `TestPermissiveIgnoresRawSubjects`: `auto_approve: ["bash:timeout *"]` does not approve `timeout 5 rm -rf a/b` in auto or smart; `auto_approve: ["bash:timeout"]` does not approve `timeout 5 rm -rf ~` in smart (floor) and never did in auto.
- `TestSplitSegments_DashCInnerRawIsOuterCall`: `bash -c "rm x && ls"` yields two segments, both with `Raw == "bash -c \"rm x && ls\""`, names `rm` and `ls`.
- Existing allow fixtures (`TestMatchesPermissive`, `TestSmartMode_AllowPatternsStayStrict`, `TestEvalRules_*`) are byte-identical except the new catastrophic rows.

`cmd/fuse`:

- `TestMCPServerGateBindsWorkspaceRoot`: build the mcp-server gate exactly as `runMCPServer` does (factor the `permissions.New` call into `buildMCPServerGate(cfg, toolReg, approve)` so the test and the binding share it), chdir into `<tmp>/ws`, mode auto, policy provenance, and execute `rm -rf ..`. Assert a `deny` at `LayerRules` and that `<tmp>` still contains `ws`. Repeat in mode smart and assert the same deny.
- `TestHostedBindingDeniesCatastrophicInSmartMode`: `buildGate` under `markPolicyApproval`, mode smart, `auto_approve: ["bash:rm"]`, sentinel directory `<tmp>/ws/keep`; execute `rm -rf ..` from `<tmp>/ws`. Assert the deny and that `keep` survives.
- `TestLoopServerDeniesWrapperRuleEndToEnd` (end-to-end, production loop-server wiring): `harnessServer(t, harnessOperator + "permissions:\n  mode: auto\n  auto:\n    deny: [\"bash:timeout*\"]\n")`; `gw.script` is a `bash` call `timeout 5 touch ran` followed by a final text reply. Drain `Observe`, `Wait`, fold in `Attach(..., 0)`. Assert one `permission.decision` with `Verdict == "deny"`, `Layer == permissions.LayerRules`, `Mode == "auto"`, reason starting `denied by auto-mode rules layer:` and containing `timeout 5 touch ran`; assert no `allow` decision for that call; assert `gw.calls[1].Messages` has a `tool`-role message carrying that reason. A second run with `mode` omitted (smart) and `always_prompt: ["bash:timeout*"]` asserts a `deny`/`LayerRules`/`policy` decision naming `bash:timeout*` (the previous change's path, now reached through the raw subject).
- `TestLoopServerDeniesCatastrophicInSmartModeEndToEnd`: `harnessServer` with `permissions:\n  auto_approve: [\"bash:rm\"]\n` (smart), script `rm -rf ..`, then final. Assert a `deny`/`LayerRules`/`policy` decision whose reason starts `catastrophic command denied by rules floor:` and `Mode == "smart"`. The harness binds no sandbox, so the decision event plus the hosted-binding test are the proof of non-execution.

## Compatibility

Tightenings, all in the safe direction, each for the release note:

- Smart-mode `auto_approve` first-token rules (`bash:rm`, `bash:dd`, `bash:mkfs*`, `bash:shutdown`) no longer approve a catastrophic segment: interactive users get a prompt, headless smart loops get a deny with a reason. Benign commands under the same rules are unaffected.
- Headless smart gates (loop servers, mcp-server without a HITL socket) now deny catastrophic commands they previously policy-approved.
- Protective rules naming a wrapper (`bash:timeout*`, `bash:nice *`, `bash:env *`, `bash:bash *`) start firing in auto mode and, for the argument-bearing forms, in smart mode. A rule written that way was already the operator's intent; it may fire more often than before, which is the direction protective rules err.
- The mcp-server gate now scopes against its cwd; `rm -rf ..` and other workspace-ancestor deletions are denied there as everywhere else.
- No config keys change. Allow matching is not widened anywhere.

## One change or two

Build this after the smart-mode always_prompt change, stacked on it, not merged with it. It reuses that change's pattern-returning matcher and adds a second rules-layer deny to the same smart branch; keeping them separate keeps the always_prompt release note (hosted behavior change) distinct from these three tightenings, and keeps each ADR about one decision.
