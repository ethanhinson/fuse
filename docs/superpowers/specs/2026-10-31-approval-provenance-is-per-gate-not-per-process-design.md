<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0094 — Approval provenance is per gate, not per process](../../changes/archive/2026-10-31-0094-approval-provenance-is-per-gate-not-per-process.md)**
<!-- docket:backlink:end -->

# Approval provenance is per gate, not per process

Designed against the permissions package and the cmd/fuse composition roots as they stand once the floor-and-wrapper-parity change (PR #116) has merged. Line numbers are approximate for that state and are cited from that branch; the same lines exist on main today with small offsets.

## Problem

Who answers a gate's approval prompts is bound per gate: every `permissions.New` call takes its own `approve` function (`internal/permissions/gate.go` around 288-299) and `CloneForChild` wraps the parent's (`gate.go` around 1005-1012). But the label that says who that function is, `approvalProvenance`, is a process-wide variable in the binding layer: `gateApprovalProvenance` defaults to human and `markPolicyApproval()` flips it for the whole process (`cmd/fuse/approval.go` around 23-33). `buildGate` stamps that one global on every gate it builds, whatever `approve` it was handed (`cmd/fuse/run.go` around 414-419). Nothing ties the label to the function.

Three consequences.

1. The research probe never calls `markPolicyApproval` (`cmd/fuse/research_probe.go` has no such call) yet builds its root and every child with `permissions.AlwaysApprove` (`cmd/fuse/runtime_binding.go` around 405, 410, 504). Every one of those gates therefore carries human provenance while a stand-in answers it. An explicit ask rule (`auto.ask`, `always_prompt`) in a probe config reaches `resolveAuto`, sees `approvalProvenance == DecidedByHuman`, returns `VerdictAsk` (`internal/permissions/gate.go` around 745-760), falls to the human block, and `AlwaysApprove` approves it. The decision trail records `ask` at `rules` then `allow` at `human` with `decided_by: human`. No human was there. The same holds for the smart-mode `always_prompt` path (`gate.go` around 598-607) and for the smart-mode catastrophic floor (`gate.go` around 580-585), which both key on the same field: in the probe, a stand-in approves a catastrophic command and the trail blames a person.

2. The shell's orchestrator in `orchestrate` mode is built with an inline deny stand-in (`cmd/fuse/orchestrate_cli.go` around 142-146) in a process whose provenance is human. Its denials are recorded as `tool call denied by user`, `decided_by: human`. Again no human answered.

3. The label is a mutable global, so every test that exercises a policy binding must save, set and restore it (`cmd/fuse/explicit_ask_binding_test.go` around 24-26, 73-75, 136-138, 189-191), and a binding that binds a stand-in but forgets the call (as the probe does) is silently mislabelled. `hitl.ClientApprovalFunc("")` returns `AlwaysApprove` (`internal/hitl/hitl.go` around 140-143); the headless mcp-server happens to call `markPolicyApproval` next to it (`cmd/fuse/mcp_server.go` around 52-58), but nothing enforces that pairing.

The explicit-ask ADR lists policy stand-ins by binding. The unit is wrong: a stand-in is an approver, and a process can hold more than one.

## Design

Provenance travels with the approver. An approver is a value that carries both the function and who it is, and a gate takes its provenance from that value and nowhere else.

### `internal/permissions`

- Add
  ```go
  // Approver is who answers a gate's prompts: the function and its provenance.
  type Approver struct {
      Answer    ApprovalFunc
      DecidedBy string // DecidedByHuman or DecidedByPolicy
  }
  func Human(fn ApprovalFunc) Approver  { return Approver{Answer: fn, DecidedBy: DecidedByHuman} }
  func Policy(fn ApprovalFunc) Approver { return Approver{Answer: fn, DecidedBy: DecidedByPolicy} }
  ```
- `AlwaysApprove` becomes a value, not a function: `var AlwaysApprove = Policy(alwaysApprove)`, where `alwaysApprove` is today's body. Every call site that passes `permissions.AlwaysApprove` keeps compiling once the parameter type changes; its provenance is now policy by construction.
- `New(cfg, inner, approve Approver, opts ...Option)`: sets `g.approve = approve.Answer` and `g.approvalProvenance = approve.DecidedBy`. A zero `DecidedBy` is rejected by panic with a message naming the constructor to use; a nil `Answer` likewise. There is no default any more: a caller must say who answers.
- Delete `WithApprovalProvenance`. It is the hole: an option that can contradict the approver it is paired with.
- `PrefixApproval(label string, a Approver) Approver` wraps `a.Answer` with `prefixedApprove` and keeps `a.DecidedBy`. `CloneForChild` is unchanged in effect: it already copies `approvalProvenance` and wraps `approve`, and both now come from the one `Approver` the gate was built with.
- `ApprovalFunc` stays as the function type; `ApprovalRequest` is untouched.

### `cmd/fuse`

- Delete `gateApprovalProvenance` and `markPolicyApproval` (`approval.go`). `buildGate` stops appending a provenance option (`run.go` around 419); the gate takes it from the approver it is given.
- `oneShotApprovalFunc` returns `permissions.Approver`: `--approve-all` yields `Policy(autoApprove)`, a TTY yields `Human(terminalApproval(in, out))`, piped stdin yields `Policy(denyNonInteractive)` (`approval.go` around 48-56). The `if *approveAll || !oneShotInteractive { markPolicyApproval() }` block in `main.go` (around 177-181) is deleted; `--approve-all` still sets mode off.
- `internal/hitl.ClientApprovalFunc(socketPath) permissions.Approver`: empty path returns `permissions.AlwaysApprove`; a socket returns `Human(dialer)`. The mcp-server's `markPolicyApproval` call goes away (`mcp_server.go` around 52-58); `buildMCPServerGate` takes an `Approver`.
- `tui.NewTeaApprovalFunc` keeps returning an `ApprovalFunc`; the shell wraps it once: `childBaseApprove := permissions.Human(tui.NewTeaApprovalFunc(m.Channel()))` (`shell.go` around 327).
- The orchestrator's inline deny becomes `permissions.Policy(deny)` (`orchestrate_cli.go` around 143).
- Loop servers drop `markPolicyApproval()` (`loop_server.go` around 59-60, `loop_serve_net.go` around 322-323) and keep `permissions.AlwaysApprove`.
- The research probe keeps passing `permissions.AlwaysApprove` and is thereby policy for the first time, which is the behavior change this spec exists for.
- Every `approve permissions.ApprovalFunc` parameter on the agent builders (`buildGate`, `buildChildAgent`, `buildAgentCore`, `buildAgentWithRendererAndTrace`, `buildMCPServerGate`, the deps-input structs' `childApprove`/`rootApprove`, `loopApprovalFor`, `newCLIAdapter`) becomes `permissions.Approver`. `loopApprovalFor`'s nil check becomes `approve.Answer == nil`; `newCLIAdapter` stores `approve.Answer`. This is mechanical; the compiler finds every site.

### Event shape

Unchanged. `decided_by` on `permission.decision` keeps its two values and its meaning; it is simply correct now for gates whose approver is a stand-in in a process that also holds a human approver, and vice versa.

### Docs

The README loop-server and permissions paragraphs that say "when the binding has no human" are reworded to "when the gate's approver is a policy stand-in (the loop servers, the headless mcp-server, the research probe, piped stdin, the orchestrator's deny stand-in)". The explicit-ask ADR's binding list is corrected by the ADR below.

### ADR

Record one ADR, status Accepted, superseding the explicit-ask ADR (ADR-0066), which is marked Superseded. It restates that ADR's decisions unchanged (an explicit ask match is its own verdict in auto and smart; with a human it prompts; with a stand-in it is denied at the rules layer with `matched ask rule <pattern>; no approver bound`; no-match behavior is unchanged) and replaces the "policy stand-in bindings" bullet with:

- Provenance is a property of the approver, and a gate's provenance is its approver's. A process may hold human and policy approvers at once; each gate reports its own.
- Policy stand-ins are `permissions.AlwaysApprove` wherever it is bound (both loop servers, the headless mcp-server, the research probe, any child built with it), the piped-stdin deny fallback, `--approve-all`'s approver (moot, since that run is mode off), and the orchestrator's deny stand-in. Human approvers are the TUI channel, the one-shot terminal prompt, and the mcp-server's HITL socket client.
- Alternatives: recognising `AlwaysApprove` by function identity (rejected: wrappers such as `PrefixApproval` erase it); keeping the process global and adding the missing `markPolicyApproval` calls (rejected: the next binding forgets again, and a process with both kinds of approver cannot be labelled by one global); a per-binding option on `buildGate` (rejected: still two values that can disagree).

## Testing

`internal/permissions`:

- `TestNew_ProvenanceIsTheApprovers`: mode auto, `auto.ask: ["bash:touch*"]`. `New(cfg, reg, AlwaysApprove)` with no options executes `touch x`: one `deny` at `LayerRules`, `DecidedBy == DecidedByPolicy`, reason `matched ask rule bash:touch*; no approver bound`. `New(cfg, reg, Human(recorder))` executes the same: the recorder is called, decisions are `ask`/`LayerRules` then `allow`/`LayerHuman`/`DecidedByHuman`.
- `TestNew_RejectsUnlabelledApprover`: `New(cfg, reg, Approver{Answer: AlwaysApprove.Answer})` panics; `New(cfg, reg, Human(nil))` panics.
- `TestCloneForChild_KeepsApproverProvenance`: a gate built with `Human(recorder)` cloned for a child prompts through the recorder with the `[child]` prefix and records `DecidedByHuman`; a gate built with `AlwaysApprove` cloned for a child denies the explicit ask with `DecidedByPolicy`.
- `TestPrefixApproval_KeepsProvenance`: `PrefixApproval("w", AlwaysApprove).DecidedBy == DecidedByPolicy`; `PrefixApproval("w", Human(fn)).DecidedBy == DecidedByHuman`; the prefix reaches the wrapped function.
- `TestSmartMode_PolicyStandIn_DeniesCatastrophic` and `TestSmartMode_AlwaysPrompt_PolicyStandInDenies` drop their `WithApprovalProvenance(DecidedByPolicy)` option and rely on `AlwaysApprove` alone; every other fixture that passed a bare recorder function wraps it in `Human(...)`. No assertion changes.

`cmd/fuse`:

- `TestOneShotApprovalFuncProvenance`: `oneShotApprovalFunc(true, true, ...)` is policy; `(false, true, ...)` is human; `(false, false, ...)` is policy and its `Answer` returns the non-interactive error.
- `TestHumanAttendedRunWithAlwaysApproveChild` (the record's named test): one process, no global. Build a root gate with `buildGate(cfg, toolReg, permissions.Human(recorder), nil, nil, nil)` and a child gate with `buildGate(cfg, toolReg, permissions.AlwaysApprove, nil, nil, nil)`, mode auto, `auto.ask: ["bash:touch*"]`, chdir into a temp dir. The root's `touch ran-root` prompts the recorder (which approves) and the file exists. The child's `touch ran-child` is denied with one `deny`/`LayerRules`/`DecidedByPolicy` decision naming `bash:touch*`, and the file does not exist. Repeat with mode smart and `always_prompt: ["bash:touch*"]`: same outcome.
- `TestMCPServerGateProvenanceFollowsSocket`: `buildMCPServerGate(cfg, toolReg, hitl.ClientApprovalFunc(""))` denies an explicit ask with `DecidedByPolicy`; with a socket path whose server approves, the same call prompts and records `DecidedByHuman`.
- `TestOrchestratorStandInIsPolicy`: build the orchestrator agent exactly as `orchestrate_cli.go` does and execute a prompt-all call; the `deny` at `LayerHuman` carries `DecidedByPolicy`.
- `TestResearchProbeGatesArePolicy`: build the probe deps through `buildResearchProbeRuntimeDeps` with a config carrying `auto.ask: ["bash:touch*"]`, run one child through its child builder with a scripted gateway that issues `touch ran`, and assert the child's `permission.decision` is `deny`/`rules`/`policy` and the sentinel is absent.
- The four tests in `explicit_ask_binding_test.go` and the three in `permission_floor_binding_test.go` lose their save/set/restore prologue. Their assertions are unchanged: the production wiring now yields policy provenance with nothing set.
- `TestLoopServerChildGateIsPolicyEndToEnd` (end-to-end, production loop-server wiring): `harnessServerWith(t, hostedOperatorYAML + "permissions:\n  mode: auto\n  auto:\n    ask: [\"bash:touch*\"]\n", raw)` over a `routedGateway`: `ROOT-TASK` turn 0 replies `spawn_agent {label: "w", task: "CHILD-TASK"}`, later root turns reply text; `CHILD-TASK` turn 0 replies a `bash` call `touch ran`, later child turns reply text. `StartLoop`, `Wait`, then `Attach(..., 0)` for the durable history. Assert exactly one `permission.decision` whose `NodeID` is the child's node (found through the captured tree by label `w`) with `Verdict == "deny"`, `Layer == permissions.LayerRules`, `DecidedBy == permissions.DecidedByPolicy`, reason naming `bash:touch*`; assert no `allow` decision for that node; assert the child's turn-1 request carries a `tool`-role message containing `matched ask rule bash:touch*`. The harness binds no sandbox; the decision event plus the in-process test above are the proof of non-execution.

## Compatibility

- `permissions.New`, `PrefixApproval`, `hitl.ClientApprovalFunc` and `oneShotApprovalFunc` change signature; `WithApprovalProvenance`, `gateApprovalProvenance` and `markPolicyApproval` are removed. `permissions.AlwaysApprove` becomes a value. External Go callers of the permissions package must wrap their function in `Human` or `Policy`; the compiler enforces it.
- Behavior change, for the release note: in `fuse research-probe`, an explicit ask rule (`auto.ask`, `always_prompt`) and, in smart mode, a catastrophic command are now denied at the rules layer with `decided_by: policy` instead of being approved by the stand-in and recorded as human. Operators who want the probe to run such commands move the pattern to `auto_approve` or drop the rule.
- Orchestrator stand-in denials now record `decided_by: policy`. Consumers that filtered on `human` for those events should filter on the `deny` verdict.
- No config key changes. No change to any hosted binding's observable behavior: both loop servers and the headless mcp-server already reported policy; they now do so by construction.

## Dependencies and order

Build after the floor-and-wrapper-parity change (PR #116) merges, because the smart-mode floor path this spec relabels lives there. The unparseable-command change (0096) builds on this one and should be stacked on it: its policy deny is only correct for a child gate when that child's provenance is its own. The worker-tools change (0095) is independent of both.
