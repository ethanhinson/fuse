<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0094 — Approval provenance is per gate, not per process](../../changes/archive/2026-10-31-0094-approval-provenance-is-per-gate-not-per-process.md)**
<!-- docket:backlink:end -->

# Approval provenance is per gate, not per process - Implementation Plan

> Executed by `docket-build`: one task at a time, each task TDD (failing test, minimal code, passing test, one commit). Every test uses scripted fakes only: recorder approval funcs, `httptest` gateways (`routedGateway`, `recordingGateway`), a host-handler sandbox in a temp dir, and an in-process HITL socket server. No live model traffic.

**Goal:** A gate's approval provenance (`human` or `policy`) comes from the approver it was built with, never from a process-wide variable. `permissions.Approver` carries the approval function and who it is. `permissions.AlwaysApprove` is a policy approver by construction, so every gate bound to it (loop-server and loop-serve-net children, the headless mcp-server, the research probe root and children) denies explicit ask rules (`auto.ask`, `always_prompt`) and, in smart mode, catastrophic commands at the rules layer with `decided_by: policy`. Human approvers (TUI channel, one-shot terminal prompt, HITL socket client) keep prompting and record `decided_by: human`. A process may hold both kinds at once and each gate reports its own.

**Architecture:**
- `internal/permissions/gate.go`: new `Approver{Answer ApprovalFunc; DecidedBy string}` with constructors `Human(fn)` and `Policy(fn)`. `AlwaysApprove` becomes `var AlwaysApprove = Policy(alwaysApprove)`. `New(cfg, inner, approve Approver, opts...)` takes provenance from the approver and panics on a missing `Answer` or a missing/unknown `DecidedBy`. `WithApprovalProvenance` is deleted. `PrefixApproval(label, Approver) Approver` keeps `DecidedBy`. `CloneForChild` is unchanged in effect.
- `cmd/fuse`: `gateApprovalProvenance` and `markPolicyApproval` are deleted. Every builder parameter that carried a `permissions.ApprovalFunc` for a gate becomes `permissions.Approver`. `oneShotApprovalFunc` and `hitl.ClientApprovalFunc` return `Approver`. The shell wraps the TUI channel in `Human(...)` once; the orchestrator's inline deny becomes `Policy(...)`.
- `internal/tui` is untouched: `tui.NewTeaApprovalFunc` and `tui.AgentBuilder` keep the bare `ApprovalFunc` type; the shell wraps at the boundary. `hitl.NewServer` keeps taking a bare `ApprovalFunc`.
- Event shape is unchanged (`decided_by` keeps its two values).

**Tech stack:** Go, `internal/permissions`, `internal/hitl`, `cmd/fuse`. Gate: `make test` and `make lint`.

**Learnings applied:**
- `security-knob-inert-at-composition-root`: the provenance fix is only real if each composition root binds the right approver. Tasks 2 to 5 assert at the binding (`buildGate`, `buildMCPServerGate`, the extracted orchestrator approver, `buildResearchProbeRuntimeDeps`, the production loop-server factory), not only in `internal/permissions`.
- `runtime-deps-field-overwrites-builder-injection` (generalized: the last writer wins): `internal/agent/loop.go` (around line 1011) installs its OWN `permissions.WithDecisionSink` on the tool-call context. A test sink on the context you pass into an agent-driven run will NOT see that agent's decisions. Agent-driven tests (Tasks 4 and 5) therefore assert through the durable event history or through the tool result the model receives at the gateway, never through a context sink.
- `scripted-gateway-double-double-escapes-tool-args`: build tool-call arguments only with the existing `toolCallsReply(call{...})` helper (it marshals once). Never hand-write pre-escaped JSON.
- `gate-as-tool-executor`: thread the new `Approver` type through the builders so tests receive the same signature as production callers.
- `shared-worktree-red-suite-verify-detached`: before trusting a red or green full-suite verdict, confirm `git status --porcelain` is clean in the feature worktree.

**Decisions made in this plan (record them in the PR body; the ADR superseding ADR-0066 is recorded separately and is not a plan task):**
1. `New` rejects a `DecidedBy` that is neither `DecidedByHuman` nor `DecidedByPolicy` (not only the empty string). Every provenance check in the gate is `== DecidedByPolicy`, so an unknown label would silently behave as human. Rejecting it fails closed at construction.
2. `newCLIAdapter` takes the `Approver` and stores `approve.Answer` (the HITL relay server it starts takes a bare function). Known limitation, out of scope for this change and worth a follow-up note in the PR body: a CLI-model gate whose approver is a policy stand-in still relays prompts to a `fuse mcp-server` subprocess that labels its socket client `human`. That was already the case before this change.
3. `TestHumanDecisionCarriesProvenance`'s second half ("Default (no option): the human posture") is rewritten, because there is no default any more: it builds with `Human(AlwaysApprove.Answer)` and asserts `DecidedByHuman`. This is the only existing assertion whose fixture changes meaning; every other existing assertion is kept byte-for-byte.
4. The orchestrator's deny stand-in is extracted into a named package-level value (`orchestrateCLIApprover`) and its agent-builder closure into a named function (`orchestrateCLIBuilder`) so the binding test exercises the production value.

**Red-check rule for Tasks 2 to 5.** Task 1 lands the production change, so the binding tests added afterwards pass on arrival. Each of those tasks therefore includes a mutation check: temporarily change `var AlwaysApprove = Policy(alwaysApprove)` to `var AlwaysApprove = Human(alwaysApprove)` (or the named approver under test to `Human(...)`), run the new test, confirm it FAILS for the provenance reason, then revert the mutation before committing. Record "mutation check: failed as expected" in the commit body. Never commit the mutation.

---

## Task 1: `permissions.Approver` and module-wide migration

**Tier:** premium (one atomic, compiler-driven signature change across the module; the risk is a fixture silently changing meaning)

**Files:**
- Modify: `internal/permissions/gate.go`, `internal/permissions/decision.go` (comment only)
- Modify: `internal/event/event.go` (comments only, around lines 76 and 433-436)
- Modify tests in `internal/permissions`: `audit_test.go`, `auto_test.go`, `clone_test.go`, `context_test.go`, `decision_test.go`, `freedom_test.go`, `gate_test.go`, `mediation_test.go`, `patternparity_test.go`, `patterns_test.go`, `safelist_test.go`, `sessionmode_live_test.go`, `sessionmode_test.go`, `spawn_safelist_test.go`, `valve_test.go`
- Create: `internal/permissions/approver_test.go`
- Modify: `internal/hitl/hitl.go`, `internal/hitl/hitl_test.go`
- Modify tests: `internal/mcp/server_test.go`, `internal/mcp/server_progress_test.go` (wrap in `permissions.Human`); `internal/mcp/progress_live_test.go`, `internal/mcp/mock_server_integration_test.go`, `internal/agent/deny_resilience_test.go`, `internal/tui/mcp_resource_subscriptions_e2e_test.go` keep compiling unchanged (they pass `permissions.AlwaysApprove`; Mode off or no ask path) - confirm they still pass
- Modify: `cmd/fuse/approval.go`, `cmd/fuse/main.go`, `cmd/fuse/run.go`, `cmd/fuse/cli_adapter.go`, `cmd/fuse/runtime_binding.go`, `cmd/fuse/loop_server.go`, `cmd/fuse/loop_serve_net.go`, `cmd/fuse/mcp_server.go`, `cmd/fuse/shell.go`, `cmd/fuse/orchestrate_cli.go`
- Modify tests in `cmd/fuse`: `approval_test.go`, `explicit_ask_binding_test.go`, `permission_floor_binding_test.go`, `mcp_server_gate_test.go`, `run_session_mode_test.go`, `run_helpers_test.go`, `shell_test.go`

**Step 1: Write failing tests** in `internal/permissions/approver_test.go` (package `permissions`; reuse `newTestRegistry`, `newApproveRecorder`, `recordDecisions`, `autoCfg`, `bashArgs`, `chdir` from the existing test files):
- `TestNew_ProvenanceIsTheApprovers`: `cfg := autoCfg(config.AutoConfig{Ask: []string{"bash:touch*"}}, nil, nil)`. Run in a temp dir (`chdir(t, canon)` of an `EvalSymlinks`'d `t.TempDir()`, `WithWorkspaceRoot(canon)`).
  - `New(cfg, newTestRegistry("bash"), AlwaysApprove, WithWorkspaceRoot(canon))` with NO provenance option executes `bashArgs("touch x")`: exactly one decision, `Verdict == "deny"`, `Layer == LayerRules`, `DecidedBy == DecidedByPolicy`, `Reason == "matched ask rule bash:touch*; no approver bound"`.
  - `approve, called := newApproveRecorder(true)`; `New(cfg, newTestRegistry("bash"), Human(approve), WithWorkspaceRoot(canon))` executes the same: `*called` is true; decisions are `ask`/`LayerRules` then `allow`/`LayerHuman`/`DecidedByHuman`.
- `TestNew_RejectsUnlabelledApprover`: a helper `mustPanic(t, name, func())` using `recover()`. Each of these panics: `New(cfg, reg, Approver{Answer: AlwaysApprove.Answer})`, `New(cfg, reg, Human(nil))`, `New(cfg, reg, Policy(nil))`, `New(cfg, reg, Approver{Answer: AlwaysApprove.Answer, DecidedBy: "robot"})`. Assert the recovered value's string names `permissions.Human` or `permissions.Policy` (so the message tells the caller which constructor to use).
- `TestCloneForChild_KeepsApproverProvenance`: same ask config. (a) A recorder that captures `req.Preview`; `parent := New(cfg, reg, Human(rec), ...)`; `child := parent.CloneForChild("w")`; `child.Execute(ctx, "bash", bashArgs("touch x"))`: the recorder was called with a preview starting `"[w] "`, and the final decision is `allow`/`LayerHuman`/`DecidedByHuman`. (b) `parent := New(cfg, reg, AlwaysApprove, ...)`; its child denies the same call with one `deny`/`LayerRules`/`DecidedByPolicy` decision.
- `TestPrefixApproval_KeepsProvenance`: `PrefixApproval("w", AlwaysApprove).DecidedBy == DecidedByPolicy`; `PrefixApproval("w", Human(fn)).DecidedBy == DecidedByHuman`; calling `PrefixApproval("w", Human(fn)).Answer(ctx, ApprovalRequest{Preview: "p"})` reaches `fn` with `Preview == "[w] p"`.

**Step 2: Run, confirm red:** `go test ./internal/permissions -run 'TestNew_|TestCloneForChild_KeepsApprover|TestPrefixApproval_Keeps' -count=1` (compile failure on `Approver`, `Human`, `Policy` is the expected red).

**Step 3: Implement in `internal/permissions/gate.go`:**
- Replace the `AlwaysApprove` function with:
  ```go
  // Approver is who answers a gate's prompts: the function and its provenance.
  // A gate's provenance is its approver's and nothing else's, so a process can
  // hold human and policy approvers at once and each gate reports its own.
  type Approver struct {
      Answer    ApprovalFunc
      DecidedBy string // DecidedByHuman or DecidedByPolicy
  }

  // Human labels fn as a person at a prompt (the TUI channel, the one-shot
  // terminal prompt, the HITL socket client).
  func Human(fn ApprovalFunc) Approver { return Approver{Answer: fn, DecidedBy: DecidedByHuman} }

  // Policy labels fn as a stand-in that answers with no human present.
  func Policy(fn ApprovalFunc) Approver { return Approver{Answer: fn, DecidedBy: DecidedByPolicy} }

  // AlwaysApprove is the headless policy stand-in: it approves every prompt and
  // is policy provenance wherever it is bound.
  var AlwaysApprove = Policy(alwaysApprove)

  func alwaysApprove(_ context.Context, _ ApprovalRequest) (bool, bool, error) {
      return true, false, nil
  }
  ```
  The `DecidedByHuman`/`DecidedByPolicy` constants already exist (around line 246); update their comment to drop the `WithApprovalProvenance` reference ("DecidedBy values for Decision.DecidedBy and Approver.DecidedBy").
- Delete `WithApprovalProvenance` and its comment.
- `New(cfg config.PermissionsConfig, inner *tools.Registry, approve Approver, opts ...Option) *PermissionGate`: before building the struct,
  ```go
  if approve.Answer == nil {
      panic("permissions.New: approver has no Answer func; build it with permissions.Human or permissions.Policy")
  }
  if approve.DecidedBy != DecidedByHuman && approve.DecidedBy != DecidedByPolicy {
      panic(fmt.Sprintf("permissions.New: approver provenance %q is not human or policy; build it with permissions.Human or permissions.Policy", approve.DecidedBy))
  }
  ```
  then `approve: approve.Answer, approvalProvenance: approve.DecidedBy`. Update the doc comment: the caller must say who answers; there is no default.
- Update the `approvalProvenance` field comment (around lines 92-98): set by `New` from the approver; never changed after; copied by `CloneForChild`.
- `PrefixApproval(label string, a Approver) Approver { return Approver{Answer: prefixedApprove(label, a.Answer), DecidedBy: a.DecidedBy} }`. `prefixedApprove` and `CloneForChild` stay as they are.
- Comments in `decision.go` (around line 21 and 36-40) and `event.go` (around 76, 433-436): replace any mention of `WithApprovalProvenance` or "binding" provenance with "the gate's approver". No code change in `event`.

**Step 4: Migrate `internal/permissions` tests.** Rules (apply exactly; do not change any assertion except Decision 3):
- `New(..., AlwaysApprove, ...)` with no provenance option: unchanged.
- `New(..., AlwaysApprove, ..., WithApprovalProvenance(DecidedByPolicy))`: drop the option (sites: `audit_test.go` ~106, `patternparity_test.go` ~115, `gate_test.go` ~639 in `TestSmartMode_PolicyStandIn_UnmatchedStillAllows`, `auto_test.go` ~722, ~746, ~789).
- `New(..., fn, ..., WithApprovalProvenance(DecidedByPolicy))` where `fn` is a recorder or local func: `Policy(fn)`, drop the option (`gate_test.go` ~514 `TestSmartMode_PolicyStandIn_DeniesCatastrophic`, ~613 `TestSmartMode_AlwaysPrompt_PolicyStandInDenies`, ~654).
- `New(..., fn, ..., WithApprovalProvenance(DecidedByHuman))`: `Human(fn)`, drop the option (`gate_test.go` ~566, `auto_test.go` ~692, ~702, ~770, `patterns_test.go` ~238).
- `WithApprovalProvenance(tc.provenance)` (`patterns_test.go` ~327): `Approver{Answer: approve, DecidedBy: tc.provenance}`.
- Every other `New(..., fn, ...)` with a bare function (`approve`, `deny`, `rec.approve`, `tc.ap...`, inline literals): `Human(fn)`. This keeps the old implicit human default for those fixtures.
- `audit_test.go` `TestHumanDecisionCarriesProvenance`: second half builds `g2` with `Human(AlwaysApprove.Answer)`; rename its comment to "Human approver: the human posture." (Decision 3).
- Any test that invokes `AlwaysApprove(...)` as a function call: use `AlwaysApprove.Answer(...)`.
- Re-derive the site list before editing with `grep -n 'WithApprovalProvenance\|AlwaysApprove' internal/permissions/*_test.go`; do not trust this plan's line numbers.

**Step 5: Migrate `internal/hitl`.** `ClientApprovalFunc(socketPath string) permissions.Approver`: empty path returns `permissions.AlwaysApprove`; otherwise `permissions.Human(func(...) {...})` around the existing dialer body. Update the doc comment (an empty path is the policy stand-in; a socket is a human in the parent TUI). Fix the header comment around line 62 ("pass `permissions.AlwaysApprove.Answer` for non-interactive sessions"). In `hitl_test.go`: `client := ClientApprovalFunc(sock)` then call `client.Answer(...)`; `NewServer(sock, permissions.AlwaysApprove)` becomes `NewServer(sock, permissions.AlwaysApprove.Answer)`. Add `TestClientApprovalFuncProvenance`: `ClientApprovalFunc("").DecidedBy == permissions.DecidedByPolicy`, `ClientApprovalFunc(sock).DecidedBy == permissions.DecidedByHuman`.

**Step 6: Migrate `cmd/fuse` production code** (compiler-driven; `permissions.AlwaysApprove` call sites keep compiling as a value):
- `approval.go`: delete `gateApprovalProvenance` and `markPolicyApproval` and their comments. `oneShotApprovalFunc(...) permissions.Approver`: `approveAll` -> `permissions.Policy(autoApprove)`; `isTTY` -> `permissions.Human(terminalApproval(in, out))`; otherwise `permissions.Policy(denyNonInteractive)`. Update its doc comment to state each branch's provenance.
- `main.go` (around 177-181): delete the `if *approveAll || !oneShotInteractive { markPolicyApproval() }` block and its comment. Keep `cfg.Permissions.Mode = "off"` under `--approve-all`.
- `run.go`: `loopApprovalFor(approve permissions.Approver, interactive bool)` with `if !interactive || approve.Answer == nil { return nil }` and `approve.Answer(ctx, ...)` inside. `buildGate(cfg, toolReg, approve permissions.Approver, ...)`: delete the `WithApprovalProvenance(gateApprovalProvenance)` append and its comment. `buildAgentWithRendererAndTrace`, `buildChildAgent`, `buildAgentCore`: `approve permissions.Approver`.
- `cli_adapter.go`: `newCLIAdapter(fuseExe string, approve permissions.Approver) *CLIAdapter` storing `approve.Answer` in the existing `approve permissions.ApprovalFunc` field (Decision 2).
- `runtime_binding.go`: `buildOneShotRuntimeDeps`'s `rootApprove permissions.Approver`; `shellDepsInput.childApprove` and `.rootApprove` become `permissions.Approver`. The research-probe sites (around lines 356, 361, 444) keep passing `permissions.AlwaysApprove` unchanged; update their comments ("headless (no TTY, AlwaysApprove)") to say the probe's gates are policy provenance. Update the doc comment around 282-292 the same way.
- `loop_server.go`: `rootApprove permissions.Approver` in `buildLoopServerRuntimeDeps`, `buildLoopServerRuntimeDepsWithObserver`, `buildLoopServerRuntimeDepsStrict`, `assembleLoopServerRuntimeDeps`, and `loopServerHost.rootApprove`. Delete `markPolicyApproval()` (around line 60).
- `loop_serve_net.go`: delete `markPolicyApproval()` (around line 323).
- `mcp_server.go`: `var approve permissions.Approver`; socket -> `hitl.ClientApprovalFunc(socketPath)`; else `permissions.AlwaysApprove` with no `markPolicyApproval()`. `buildMCPServerGate(cfg, toolReg, approve permissions.Approver)`: drop the `WithApprovalProvenance` option.
- `shell.go`: the `build` closure keeps the `tui.AgentBuilder` signature (`approve permissions.ApprovalFunc`) and passes `permissions.Human(approve)` into `buildAgentWithRendererAndTrace`. `childBaseApprove := permissions.Human(tui.NewTeaApprovalFunc(m.Channel()))`; it already flows into `shellDepsInput` and the shell orchestrator's `buildChildAgent` (around line 377).
- `orchestrate_cli.go` (around 142-146): `deny` becomes `permissions.Policy(func(context.Context, permissions.ApprovalRequest) (bool, bool, error) { return false, false, nil })`. (Task 3 extracts it.)
- Comments in `loop_server.go` (~51, ~105, ~400) and `loop_serve_net.go` (~225) that describe auto-approve: keep the meaning, and say the gates are policy provenance because `AlwaysApprove` is a policy approver.

**Step 7: Migrate `cmd/fuse` tests:**
- `approval_test.go`: each `fn := oneShotApprovalFunc(...)` call site calls `fn.Answer(...)`.
- `explicit_ask_binding_test.go` (4 tests), `permission_floor_binding_test.go` (every test carrying the prologue, currently 5 sites including subtests), `mcp_server_gate_test.go` (`TestMCPServerGateBindsWorkspaceRoot`): delete each `prev := gateApprovalProvenance` / `t.Cleanup(...)` / `markPolicyApproval()` prologue. Assertions unchanged. Update the doc comments that say "buildGate under markPolicyApproval + AlwaysApprove" to "buildGate with AlwaysApprove, a policy approver".
- `mcp_server_gate_test.go` `mcpWriteLayers`: `buildMCPServerGate(cfg, toolReg, permissions.Human(deny))` (its comment already says human-bound).
- `run_session_mode_test.go`: `approveAllFunc` keeps returning `permissions.ApprovalFunc`; its five `buildGate` call sites pass `permissions.Human(approveAllFunc(&prompted))`.
- `shell_test.go` (~81): `permissions.Human(approve)` into `buildAgentWithRendererAndTrace`.
- `run_helpers_test.go` (~67): `loopApprovalFor(permissions.Policy(autoApprove), got)`.
- All other `permissions.AlwaysApprove` arguments in `cmd/fuse` tests compile unchanged.
- Add to `approval_test.go`: `TestOneShotApprovalFuncProvenance`: `oneShotApprovalFunc(true, true, ...)` has `DecidedBy == permissions.DecidedByPolicy`; `(false, true, ...)` is `DecidedByHuman`; `(false, false, ...)` is `DecidedByPolicy` and its `Answer(ctx, permissions.ApprovalRequest{Preview: "x"})` returns `approved == false` and an error containing `non-interactive`.

**Step 8: Verify.**
- `go build ./... && go vet ./...`
- `grep -rn --include='*.go' 'WithApprovalProvenance\|markPolicyApproval\|gateApprovalProvenance' .` prints nothing.
- `go test ./internal/permissions ./internal/hitl ./internal/mcp ./internal/agent ./internal/tui ./cmd/fuse -count=1`. All pass. If an existing test fails, it is a fixture whose meaning changed (a bare `AlwaysApprove` that was relying on the old human default on an explicit-ask or smart catastrophic path). Fix the FIXTURE by binding the approver the test meant (`Human(...)` for a test about prompting, `AlwaysApprove` for a test about headless denial), never the assertion, and list each such fixture in the commit body. If an assertion itself seems wrong, stop and return BLOCKED with the test name.

**Step 9: Commit** `fix(permissions): take approval provenance from the gate's approver`
Body: one line per migrated binding; the fixture list from Step 8 if any. No co-author trailer.

---

## Task 2: Human-attended run with an always-approve child

**Tier:** standard

**Files:**
- Create: `cmd/fuse/approval_provenance_binding_test.go`

**Step 1: Write the test** `TestHumanAttendedRunWithAlwaysApproveChild` (the record's named test). Shape it after `TestHostedBindingDeniesExplicitAskRule` in `explicit_ask_binding_test.go`: host-handler sandbox (`sandbox.NewService(sandbox.Config{Contained: false, Handler: sandbox.HandlerHost})`), `toolReg := defaultToolRegistry(svc, cfg.Research, nil)`, `t.Chdir(t.TempDir())`, decisions via `permissions.WithDecisionSink` (safe here: no agent is involved, the test calls `gate.Execute` directly). No global is touched anywhere in the test.
- Table over two configs:
  - `auto`: `config.PermissionsConfig{Mode: "auto", Auto: config.AutoConfig{Ask: []string{"bash:touch*"}}}`
  - `smart`: `config.PermissionsConfig{Mode: "smart", AlwaysPrompt: []string{"bash:touch*"}}`
- For each: a recorder approval func that approves and counts calls. `root := buildGate(cfg, toolReg, permissions.Human(recorder), nil, nil, nil)` and `child := buildGate(cfg, toolReg, permissions.AlwaysApprove, nil, nil, nil)`, both in the same test (the same process).
  - `root.Execute(ctxRoot, "bash", argsJSON(map[string]any{"command": "touch ran-root"}))`: not denied; the recorder was called once; `ran-root` exists; the root's decisions end with `allow`/`LayerHuman`/`DecidedByHuman`.
  - `child.Execute(ctxChild, "bash", argsJSON(map[string]any{"command": "touch ran-child"}))`: `res.Denied`; the recorder call count is unchanged; `ran-child` does not exist; the child's decisions are exactly one `deny`/`LayerRules`/`DecidedByPolicy` whose reason contains `bash:touch*`; there is no `allow` decision for the child.
- Use separate decision slices for root and child.

**Step 2: Run** `go test ./cmd/fuse -run TestHumanAttendedRunWithAlwaysApproveChild -count=1`. Passes (Task 1 is in).

**Step 3: Mutation check** (see the red-check rule): set `var AlwaysApprove = Human(alwaysApprove)` in `internal/permissions/gate.go`, rerun, confirm the child sub-tests fail (the child prompts and `ran-child` is created or the decision is `human`). Revert.

**Step 4: Commit** `test(fuse): a policy child in a human-attended run denies explicit asks`

---

## Task 3: mcp-server socket provenance and the orchestrator stand-in

**Tier:** standard

**Files:**
- Modify: `cmd/fuse/orchestrate_cli.go`
- Modify: `cmd/fuse/approval_provenance_binding_test.go`

**Step 1: Extract the orchestrator wiring** in `orchestrate_cli.go`:
```go
// orchestrateCLIApprover is the `fuse orchestrate` orchestrator's approver: a
// policy stand-in that denies every prompt. No human answers it, so its
// denials record decided_by: policy.
var orchestrateCLIApprover = permissions.Policy(func(context.Context, permissions.ApprovalRequest) (bool, bool, error) {
    return false, false, nil
})

// orchestrateCLIBuilder builds the orchestrator agent for `fuse orchestrate`.
func orchestrateCLIBuilder(cfg config.Config, reg *model.Registry, alias string, rateGate model.RateGate) func(agent.Renderer, *tools.Registry) (*agent.Agent, error) {
    return func(r agent.Renderer, orchTools *tools.Registry) (*agent.Agent, error) {
        return buildChildAgent(cfg, reg, alias, r, orchestrator.SystemPrompt(), orchTools, orchestrateCLIApprover, nil,
            "orchestrator", permissions.NewSessionMode(orchestrateCLIMode), true, rateGate, nil)
    }
}
```
with `const orchestrateCLIMode = permissions.ModeSmart`, and `newShellOrchestrator(ctx, out, target, orchestrateCLIBuilder(cfg, reg, alias, rateGate))` at the call site. Behavior is identical to Task 1's inline form.

**Step 2: Write the tests** in `approval_provenance_binding_test.go`:
- `TestMCPServerGateProvenanceFollowsSocket`: config `Mode: "auto"`, `Auto.Ask: ["bash:touch*"]`; host-handler sandbox registry; `t.Chdir` into a temp dir.
  - `buildMCPServerGate(cfg, toolReg, hitl.ClientApprovalFunc(""))` executing `touch ran-a`: one `deny`/`LayerRules`/`DecidedByPolicy`; `ran-a` absent.
  - Socket case: make the socket directory with `os.MkdirTemp("", "hitl")` (NOT `t.TempDir()`: a long test name can push a macOS unix socket path past its 104-byte limit) and `t.Cleanup(os.RemoveAll)`. `srv, err := hitl.NewServer(filepath.Join(dir, "s.sock"), func(context.Context, permissions.ApprovalRequest) (bool, bool, error) { return true, false, nil })`, `t.Cleanup(srv.Close)`. `buildMCPServerGate(cfg, toolReg, hitl.ClientApprovalFunc(sockPath))` executing `touch ran-b`: decisions `ask`/`LayerRules` then `allow`/`LayerHuman`/`DecidedByHuman`; `ran-b` exists.
- `TestOrchestratorStandInIsPolicy`:
  - `orchestrateCLIApprover.DecidedBy == permissions.DecidedByPolicy`.
  - Build the gate exactly as `buildChildAgent` does for the orchestrator: `buildGate(cfg, orchTools, orchestrateCLIApprover, nil, nil, permissions.NewSessionMode(orchestrateCLIMode))` where `orchTools` is a `tools.NewRegistry()` holding one test tool `provenanceProbeTool` (name `orch_probe`, not on the safe list; `Execute` records that it ran). `cfg` is `config.Config{}` (smart comes from the session mode). Executing `orch_probe` with `{}`: `res.Denied`; the tool did not run; the final decision is `deny`/`LayerHuman`/`DecidedByPolicy` with reason `tool call denied by user`.
  - Construction smoke: `orchestrateCLIBuilder(cfg, registryFromConfig(cfg), registryFromConfig(cfg).Default, nil)(discardRenderer{}, orchTools)` returns a non-nil agent and no error (no model call is made; nothing runs the agent).

**Step 3: Run** `go test ./cmd/fuse -run 'TestMCPServerGateProvenanceFollowsSocket|TestOrchestratorStandInIsPolicy' -count=1`. Passes.

**Step 4: Mutation checks:** (a) `ClientApprovalFunc("")` returning `permissions.Human(permissions.AlwaysApprove.Answer)` makes the empty-socket case fail; (b) `orchestrateCLIApprover = permissions.Human(...)` makes the orchestrator test fail. Revert both.

**Step 5: Commit** `test(fuse): mcp-server and orchestrator approvers carry their own provenance`

---

## Task 4: Research probe gates are policy

**Tier:** standard

**Files:**
- Modify: `cmd/fuse/approval_provenance_binding_test.go`

**Step 1: Write** `TestResearchProbeGatesArePolicy`, modeled on `TestResearchProbeRefusesNestedRootSpawn` in `workflow_activation_test.go`:
- `t.Setenv("HOME", t.TempDir())`; `dir := t.TempDir()`; `t.Chdir(dir)`.
- `gw := &routedGateway{route: ...}`: when `task` contains `CHILD-TASK` and `turn == 0`, reply `toolCallsReply(call{name: "bash", args: map[string]any{"command": "touch ran"}})`; otherwise `textReply("done")`. Serve it with `httptest.NewServer`, `t.Cleanup(srv.Close)`.
- `cfg := config.Default()`; gateway URL/key from the server; `cfg.Permissions.Mode = "auto"`; `cfg.Permissions.Auto.Ask = []string{"bash:touch*"}`.
- Host-handler sandbox; `toolReg := defaultToolRegistry(svc, cfg.Research, nil)`; tree via `agent.NewAgentTreeWithConcurrency(reg.Default, reg.Default, cfg.Agents.MaxConcurrent)`.
- `_ = buildResearchProbeRuntimeDeps(researchProbeDepsInput{cfg: cfg, reg: reg, alias: reg.Default, toolReg: toolReg, tree: tree, logSink: probe.NewLog()})` (nil scope builds a fresh one). This registers the probe root's `spawn_agent` on `toolReg` and wires the probe's own child builder.
- `res := toolReg.Execute(context.Background(), "spawn_agent", argsJSON(map[string]any{"label": "w", "task": "CHILD-TASK"}))`; the spawn itself succeeds.
- Assertions (through the gateway and the filesystem, not a context sink: the agent installs its own decision sink, see Learnings):
  - `turnRequest(t, gw, "CHILD-TASK", 0)` offers a tool named `bash` (sanity: the test cannot pass vacuously). If the probe child's palette does not include `bash`, stop and return BLOCKED with the palette; do not switch tools silently.
  - `turnRequest(t, gw, "CHILD-TASK", 1)` carries a `tool`-role message containing `matched ask rule bash:touch*; no approver bound`. That reason is produced only on the policy-provenance branch (the human branch asks instead), so it proves the child gate is policy and the deny was at the rules layer.
  - `filepath.Join(dir, "ran")` does not exist.

**Step 2: Run** `go test ./cmd/fuse -run TestResearchProbeGatesArePolicy -count=1`. Passes.

**Step 3: Mutation check:** with `var AlwaysApprove = Human(alwaysApprove)`, the test fails (the stand-in approves, `ran` is created, and the tool message has no deny reason). Revert. This is also the before-state the spec describes for the probe.

**Step 4: Commit** `test(fuse): research-probe children deny explicit asks as policy`

---

## Task 5: End to end - loop-server child gate is policy

**Tier:** standard

**Files:**
- Modify: `cmd/fuse/explicit_ask_binding_test.go` (next to the other loop-server end-to-end tests)

**Step 1: Write** `TestLoopServerChildGateIsPolicyEndToEnd` (production loop-server wiring, scripted gateway):
- `gw := &routedGateway{route: ...}`: `ROOT-TASK` turn 0 -> `toolCallsReply(call{name: "spawn_agent", args: map[string]any{"label": "w", "task": "CHILD-TASK"}})`; later `ROOT-TASK` turns -> `textReply("root done")`. `CHILD-TASK` turn 0 -> `toolCallsReply(call{name: "bash", args: map[string]any{"command": "touch ran"}})`; later `CHILD-TASK` turns -> `textReply("child done")`.
- `raw := &rawCapture{next: gw}`; `deps, host := harnessServerWith(t, hostedOperatorYAML+"permissions:\n  mode: auto\n  auto:\n    ask: [\"bash:touch*\"]\n", raw)`; `capture := &treeCapture{inner: host}`; `deps.Harness = capture`; `rt := runtime.New(deps)`.
- `h, err := rt.StartLoop(ctx, runtime.LoopConfig{Task: "ROOT-TASK"})`; `_, _ = h.Wait()`; `hist, err := rt.Attach(ctx, event.DefaultTenant, h.ID(), 0)` (durable history only; do not fold in a live tail, it can double-count).
- `tree := capture.last()`; `child := nodeLabelled(t, tree, "w")`.
- Over `hist`, collect `event.KindPermissionDecision` events whose `ev.NodeID == child.ID`, decoded as `event.PermissionDecisionPayload`:
  - exactly one with `Verdict == "deny"`, and it has `Layer == permissions.LayerRules`, `DecidedBy == permissions.DecidedByPolicy`, `Reason` containing `bash:touch*`;
  - none with `Verdict == "allow"`.
- `turnRequest(t, gw, "CHILD-TASK", 1)` carries a `tool`-role message containing `matched ask rule bash:touch*`.
- Doc comment: the harness binds no sandbox, so bash could not run here either way; the decision event plus `TestHumanAttendedRunWithAlwaysApproveChild` and `TestResearchProbeGatesArePolicy` are the proof of non-execution. Note in the comment that `harnessServerWith` never called `markPolicyApproval`, so before this change the child gate was labelled human and this test failed.

**Step 2: Run** `go test ./cmd/fuse -run TestLoopServerChildGateIsPolicyEndToEnd -count=1 -race`. Passes.

**Step 3: Mutation check:** with `var AlwaysApprove = Human(alwaysApprove)`, the test fails (the child's bash decision is `ask` then `allow`/`human`). Revert.

**Step 4: Commit** `test(fuse): loop-server children deny explicit asks as policy end to end`

---

## Task 6: README wording

**Tier:** economy

**Files:**
- Modify: `README.md`

**Steps:**
1. Around line 263: replace "when the binding has no human (the loop servers, the headless mcp-server, piped stdin)" with "when the gate's approver is a policy stand-in (the loop servers, the headless mcp-server, the research probe, piped stdin, the orchestrator's deny stand-in)". Keep the rest of the sentence (the `matched ask rule <pattern>; no approver bound` reason) unchanged.
2. Around line 277-278: replace "denies at the rules layer when only a policy stand-in is bound" with "denies at the rules layer when the gate's approver is a policy stand-in".
3. Add one sentence after those two edits, in the same paragraph: "Provenance belongs to each gate's approver, so a child built with the always-approve stand-in inside an attended session is a policy gate, and `decided_by` on its `permission.decision` events says `policy`."
4. In the `research-probe` section (around line 365), add one sentence: "The probe answers approvals with a policy stand-in: an `always_prompt` or `auto.ask` match, and in smart mode a catastrophic command, is denied with the pattern in the reason. Move the pattern to `auto_approve` or drop the rule to let it run."
5. Check: `grep -n 'binding has no human' README.md` prints nothing. Use "-" not em dashes in new text.
6. Commit `docs: approval provenance follows each gate's approver`

---

## Task 7: Final verification

**Tier:** economy

**Steps:**
1. `git -C <feature worktree> status --porcelain` is clean before running the suite.
2. `gofmt -l ./cmd ./internal` prints nothing.
3. `make lint` (runs `go vet ./...`).
4. `make test` (runs `observability-validate` then `go test ./...`). Also `go test -race ./internal/permissions ./internal/hitl ./cmd/fuse -count=1`.
5. `grep -rn --include='*.go' 'WithApprovalProvenance\|markPolicyApproval\|gateApprovalProvenance' .` prints nothing.
6. `grep -rn --include='*.go' 'permissions.ApprovalFunc' cmd/fuse | grep -v _test.go` lists only the shell's `tui.AgentBuilder` closure parameter, `cli_adapter.go`'s stored field, and `approval.go`'s helper return types (`terminalApproval`). Any builder parameter still typed `ApprovalFunc` that reaches `buildGate` is a miss.
7. If anything needed a fix, commit `chore(fuse): post-verification fixes for per-gate approval provenance`. Otherwise do not create a commit.

## Spec coverage map

| Spec item | Task |
|---|---|
| `Approver`, `Human`, `Policy`; `AlwaysApprove` a policy value | 1 |
| `New` takes an `Approver`, rejects a missing `Answer` or provenance; `WithApprovalProvenance` deleted | 1 (`TestNew_ProvenanceIsTheApprovers`, `TestNew_RejectsUnlabelledApprover`) |
| `PrefixApproval` keeps provenance; `CloneForChild` keeps the approver's provenance | 1 (`TestPrefixApproval_KeepsProvenance`, `TestCloneForChild_KeepsApproverProvenance`) |
| Smart-mode policy stand-in tests drop the option and rely on `AlwaysApprove` or `Policy(...)`; other bare fixtures wrap in `Human(...)` | 1 Step 4 |
| `gateApprovalProvenance` and `markPolicyApproval` deleted; `buildGate` stamps nothing | 1 Step 6 |
| `oneShotApprovalFunc` returns `Approver`; `main.go` block deleted | 1 Step 6, `TestOneShotApprovalFuncProvenance` |
| `hitl.ClientApprovalFunc` returns `Approver`; mcp-server drops `markPolicyApproval`; `buildMCPServerGate` takes `Approver` | 1 Step 5-6, 3 (`TestMCPServerGateProvenanceFollowsSocket`) |
| Shell wraps `tui.NewTeaApprovalFunc` in `Human` | 1 Step 6 |
| Orchestrator deny is `Policy(deny)` | 1 Step 6, 3 (`TestOrchestratorStandInIsPolicy`) |
| Loop servers drop `markPolicyApproval`, keep `AlwaysApprove` | 1 Step 6, 5 (`TestLoopServerChildGateIsPolicyEndToEnd`) |
| Research probe is policy for the first time | 4 (`TestResearchProbeGatesArePolicy`) |
| Builder signatures (`buildGate`, `buildChildAgent`, `buildAgentCore`, `buildAgentWithRendererAndTrace`, `buildMCPServerGate`, deps-input `childApprove`/`rootApprove`, `loopApprovalFor`, `newCLIAdapter`) take `Approver` | 1 Step 6, 7 step 6 |
| Human-attended run with an always-approve child (record's named test) | 2 (`TestHumanAttendedRunWithAlwaysApproveChild`) |
| `explicit_ask_binding_test.go` and `permission_floor_binding_test.go` lose the save/set/restore prologue, assertions unchanged | 1 Step 7 |
| README rewording | 6 |
| `make test` and lint gate | 7 |
| ADR superseding ADR-0066 | Not a plan task (recorded separately) |
