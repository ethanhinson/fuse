<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0098 — Permission deny reasons bound and redact the command they echo](../../changes/archive/2026-10-31-0098-permission-deny-reasons-bound-and-redact-the-command-they-ec.md)**
<!-- docket:backlink:end -->

# Permission deny reasons bound and redact the command they echo - implementation plan

Change #98 · spec: `docs/superpowers/specs/2026-10-31-permission-deny-reasons-bound-and-redact-the-command-they-ec-design.md`
Branch: `fix/permission-deny-reasons-bound-and-redact-the-command-they-ec` · base HEAD `ea836ba` (spec commit on top of main `eb16b8a`)
Executed by: `docket-build` (task-by-task, one commit per task, one full-suite gate at the end)

> **Plan-role degradation.** `superpowers:writing-plans` is not installed on this machine. Per the
> missing-skill rule the plan role degraded and this plan was authored by the plan writer directly.

## Ground rules for every task

- Tests use scripted fakes only (stub completers, `recordingGateway`, `routedGateway`, real
  `hitl.NewServer` sockets in temp dirs). Never call a real model, never call Anthropic/Claude, never
  spawn the real `claude` CLI. `CLIAdapter.Complete` is never invoked by these tests; only
  `subprocessEnv` and the adapter's construction are inspected.
- No em dashes in any new or changed prose, code comment, string literal, test name, or commit
  message; use ` - `.
- Commit messages: imperative subject, short body, **no** `Co-Authored-By` or other trailers.
- Do not mention any downstream application, hosted product, or customer anywhere (code, comments,
  README, commit messages, PR body).
- Run `gofmt -l` on touched files and `go vet ./internal/permissions/ ./cmd/fuse/` before each commit.
- Line numbers below are for `ea836ba`; locate by the quoted code, not by number, once earlier tasks
  have shifted lines.

## Reconcile notes (deviations from the spec, decided at plan time)

1. **The heuristic deny site is unreachable through the gate.** `classifyHeuristic`
   (`internal/permissions/heuristics.go`) returns only `VerdictAsk` or `VerdictAllow`; nothing
   produces `VerdictDeny`, so `"denied by auto-mode heuristic layer: " + command` (`gate.go:838`) can
   never be reached by `Execute`. The spec's "heuristic egress deny with `curl https://u:tok@evil/x`"
   table row therefore cannot be built. The site is still converted to `reasonCommand` (so a future
   heuristic deny is bounded), and the row is replaced by (a) an auto-mode unparseable command with a
   credentialed URL matched by an `auto.deny` rule (exercises `gate.go:797`, a site the spec lists but
   its table did not cover) and (b) the classifier row below. A source guard test pins that no
   reason in `gate.go` concatenates a raw `command` any more (Task 1).
2. **The classifier deny reason is included.** `classifierDenyReason(operand, ...)` (`gate.go:1119`)
   also echoes the command unbounded. The change record's "What changes" says *all* deny reasons
   that echo a command use the helper, and a credentialed `curl` that egress sends to the classifier
   is exactly the shape the spec worries about. `classifierDenyReason` wraps its operand in
   `reasonCommand`. The classifier's own rationale (`modelReason`) is model text and is left as is
   (out of scope; noted for the PR).
3. **`subprocessEnv` strips an inherited `FUSE_HITL_MODE`.** The spec says the variable is appended
   when mode off is set "and nothing otherwise". Because `subprocessEnv` starts from `os.Environ()`, a
   `fuse` that itself runs inside a relayed subprocess would otherwise pass a stale `FUSE_HITL_MODE=off`
   through to its own `claude` child. The adapter drops any inherited `FUSE_HITL_MODE=` entry and
   appends its own only when `modeOff` is set, so the relayed mode is always this process's decision.
4. **README has no `--approve-all` flag text and no mcp-server section today.** The "flag text" is the
   `-approve-all` help string in `cmd/fuse/main.go:123`; it gains the cli/ subprocess clause. The
   README sentence goes into the `[permissions]` config block section, next to the existing
   policy-stand-in paragraph (which already names "the headless mcp-server"). The existing README
   sentence that quotes `unparseable command denied; no approver bound: <command>` also gains a short
   clause saying the echoed command is bounded and redacted like the decision event's `command` field.
5. **Agent completer access in tests.** `agent.Agent` exposes no accessor for its `Completer`. The
   composition-root tests read the unexported `model` field with the same reflect/unsafe pattern
   `observerOf` already uses in `cmd/fuse/observe_wiring_test.go`. No production API is added.
6. **Metadata items in the spec are not build work.** The spec's "update 0098's title, set
   `related: [94, 96, 97]`, kill 0097" are Docket metadata operations. No build task performs them;
   they are flagged for the parent workflow.

## Task 1 - one bounded, redacted command text for every reason

Tier: standard. Files: `internal/permissions/gate.go`, new `internal/permissions/reason_bound_test.go`.

### Tests first (`internal/permissions/reason_bound_test.go`)

Package `permissions`. Reuse existing helpers: `bashArgs` (`auto_test.go`), `autoCfg` (`auto_test.go`),
`newTestRegistry` (`gate_test.go`), `recordDecisions` (`decision_test.go`), `newApproveRecorder`
(`auto_test.go`).

`TestReasonCommand_BoundsAndRedacts`, table-driven:

| input | want |
|---|---|
| `curl https://u:p@h/x` | `curl https://***@h/x` |
| `strings.Repeat("a", 10001)` | `strings.Repeat("a", 200) + "…"` |
| `"  ls  "` | `ls` |
| `ls -la` | `ls -la` (short, credential-free input is byte-identical) |

For every row also assert `reasonCommand(in) == commandPreview("bash", bashArgs(in))`, pinning that
the prose and the structured preview cannot drift.

`TestClassifierDenyReason_BoundsOperand`: `classifierDenyReason("curl https://u:tok@h/x", "x")` equals
`"denied by auto-mode classifier: curl https://***@h/x (x)"`; `classifierDenyReason(strings.Repeat("a", 5000), "")`
equals `"denied by auto-mode classifier: " + strings.Repeat("a", 200) + "…"`.

`TestDenyReasons_UseBoundedCommand`, table of `{desc, perms config.PermissionsConfig, approver Approver, command, prefix, layer, url bool}`;
each row builds `New(row.perms, newTestRegistry("bash"), row.approver, WithWorkspaceRoot(t.TempDir()))`,
executes `bash` with `bashArgs(row.command)` under `recordDecisions()`, and asserts:

- exactly one decision with `Verdict == "deny"` and `Layer == row.layer`;
- `strings.HasPrefix(d.Reason, row.prefix)`;
- `len(d.Reason) <= len(row.prefix) + 200 + len("…")`;
- `!strings.Contains(d.Reason, "tok")`, and `strings.Contains(d.Reason, "***@")` when `row.url`;
- `res.Denied` and `strings.HasSuffix(res.Output, "; "+denyHint(res.DenyLayer))`, and
  `strings.TrimSuffix(res.Output, "; "+denyHint(res.DenyLayer)) == d.Reason` (the model sees the same
  bounded reason; note the unparseable prefix itself contains `; `, so do not split on it).

Rows:

1. auto mode (`autoCfg(config.AutoConfig{}, nil, nil)`), `AlwaysApprove`, `"sudo " + strings.Repeat("a", 10000)`,
   prefix `unparseableDenyPrefix`, layer `LayerRules` (unparseable over-length, `gate.go:803`).
2. auto mode with `Auto: config.AutoConfig{Deny: []string{"bash:curl*"}}`, `AlwaysApprove`,
   `curl https://u:tok@h/x`, prefix `"denied by auto-mode rules layer: "`, layer `LayerRules`, url
   (parsed rules deny, `gate.go:813`).
3. auto mode with `Deny: []string{"bash:sudo*"}`, `AlwaysApprove`, `sudo curl https://u:tok@h/x`,
   prefix `"denied by auto-mode rules layer: "`, layer `LayerRules`, url (unparseable rules deny,
   `gate.go:797`; replaces the unreachable heuristic row, see reconcile note 1).
4. smart mode (`config.PermissionsConfig{Mode: "smart"}`), `AlwaysApprove` (policy provenance),
   `"rm -rf / " + strings.Repeat("x ", 2500)`, prefix `catastrophicFloorDenyPrefix`, layer `LayerRules`
   (smart floor deny, `gate.go:602`).
5. smart mode, `AlwaysApprove`, `"sudo " + strings.Repeat("a", 10000)`, prefix `unparseableDenyPrefix`,
   layer `LayerRules` (smart unparseable deny, `gate.go:632`).

Plus a subtest `"human-bound smart floor ask is bounded"`: smart mode, `Human(approve)` where
`approve, _ := newApproveRecorder(false)`, same `rm -rf / ...` command. Assert the first recorded
decision is `Verdict == "ask"`, `Layer == LayerRules`, `strings.HasPrefix(Reason, catastrophicAskPrefix)`,
and `len(Reason) <= len(catastrophicAskPrefix) + 200 + len("…")` (`gate.go:607`).

`TestGateReasonsNeverEchoRawCommand` (source guard for reconcile note 1): read `gate.go` with
`os.ReadFile("gate.go")` and fail if the regexp `\+ (command|operand)\b` matches any line. After this
task every echo goes through `reasonCommand(...)`, so the guard is green; a future reason added with
plain concatenation turns it red.

Run `go test ./internal/permissions/ -run 'ReasonCommand|DenyReasons_UseBounded|ClassifierDenyReason_Bounds|GateReasonsNeverEcho'`
and see them fail (undefined `reasonCommand`, unbounded reasons).

### Implementation (`internal/permissions/gate.go`)

Replace the body of `commandPreview` and add two helpers next to it:

```go
// commandPreview builds the bounded command field for a permission.decision
// event: the bash command, or the raw args for other tools, passed through
// boundText. Never full args - tool.call already carries them, and the
// decision stream must stay small (event-stream hygiene, change 0067).
// Decision events are a logged, exported audit record, so a credential-bearing
// URL must not put the secret on that record (results follow-up #2).
func commandPreview(name, args string) string {
	s := args
	if name == "bash" {
		s = bashCommand(args)
	}
	return boundText(s)
}

// boundText is the one bounding rule for command text a decision record may
// carry: trimmed, URL userinfo replaced with "***@", and capped at 200 bytes
// with a trailing "…".
func boundText(s string) string {
	s = strings.TrimSpace(redactURLUserinfo(s))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// reasonCommand is the command text every deny or ask reason that echoes a
// command uses. It is boundText, the same rule as the structured preview, so
// the prose Reason and the Command field beside it on a decision event can
// never disagree about what the record may contain.
func reasonCommand(command string) string { return boundText(command) }
```

Replace each raw echo with `reasonCommand(command)`:

- `reason := catastrophicFloorDenyPrefix + command` (smart floor deny)
- `askReason = catastrophicAskPrefix + command` (smart floor ask)
- `reason := unparseableDenyPrefix + command` (smart unparseable deny)
- `"denied by auto-mode rules layer: " + command` (both occurrences in `resolveAuto`)
- `unparseableDenyPrefix + command` (auto unparseable deny)
- `"denied by auto-mode heuristic layer: " + command`
- in `classifierDenyReason`: `r := "denied by auto-mode classifier: " + reasonCommand(operand)`

Leave untouched: `explicitAskReason` (names a pattern, not a command), `policyForcedDeny` (keys on
prefixes), `makePreview` (80-character TUI prompt line, shown to a person, not recorded),
`fetchFloorAskReason`.

### Verify

`go test ./internal/permissions/` - all green, including the unchanged exact-string tests in
`unparseable_test.go`, `patternparity_test.go`, `patterns_test.go`, `decision_test.go` (their commands
are short and carry no userinfo, so their reasons are byte-identical).

Commit: `Bound and redact the command echoed in permission deny reasons`

## Task 2 - the approver deny names its decider

Tier: standard. Files: `internal/permissions/gate.go`, `internal/permissions/policy.go`,
`internal/permissions/reason_bound_test.go` (append), `cmd/fuse/approval_provenance_binding_test.go`.

### Tests first

Append to `internal/permissions/reason_bound_test.go`:

`TestApproverDenyReason`, two subtests, each smart mode (`config.PermissionsConfig{Mode: "smart"}`),
`newTestRegistry("bash")`, `WithWorkspaceRoot(t.TempDir())`, executing `bash` with `bashArgs("make build")`
under `recordDecisions()`, with `deny := func(context.Context, ApprovalRequest) (bool, bool, error) { return false, false, nil }`:

- `Human(deny)`: the last decision is `Verdict "deny"`, `Layer LayerHuman`, `DecidedBy DecidedByHuman`,
  `Reason "tool call denied by user"`; `strings.HasPrefix(res.Output, "tool call denied by user")`;
  `res.DenyLayer == LayerHuman`.
- `Policy(deny)`: the last decision is `deny`/`LayerHuman`/`DecidedByPolicy` with
  `Reason "tool call denied by policy approver"`; `strings.HasPrefix(res.Output, "tool call denied by policy approver")`;
  `res.DenyLayer == LayerHuman`.

Add a direct unit check too: `approverDenyReason(DecidedByPolicy) == "tool call denied by policy approver"`,
`approverDenyReason(DecidedByHuman) == "tool call denied by user"`, `approverDenyReason("") == "tool call denied by user"`.

Update `cmd/fuse/approval_provenance_binding_test.go`, `TestOrchestratorStandInIsPolicy`: the final
decision's expected `Reason` becomes `"tool call denied by policy approver"` (both in the condition and
in the `t.Fatalf` message). Leave the rest of the test as is.

Run both and see them fail.

### Implementation

`internal/permissions/gate.go`, near the other reason helpers (after `catastrophicAskPrefix`):

```go
// approverDenyReason is the reason recorded and returned when the gate's
// approver answers no. It names who answered so the sentence agrees with
// decided_by, which stays the field to key on: a policy stand-in is not a user.
func approverDenyReason(decidedBy string) string {
	if decidedBy == DecidedByPolicy {
		return "tool call denied by policy approver"
	}
	return "tool call denied by user"
}
```

In `resolve`, the `if !approved {` branch:

```go
	if !approved {
		reason := approverDenyReason(g.approvalProvenance)
		g.emitDecision(ctx, mode, name, args, "deny", LayerHuman, reason, g.approvalProvenance, askClassifier)
		return ToolPolicy{Enabled: true, AutoApprove: false, DenyReason: reason, DenyLayer: LayerHuman}, nil
	}
```

In `Execute`, replace `msg := "tool call denied by user"` with `msg := approverDenyReason(g.approvalProvenance)`
(the fallback stays for any reason-less deny).

`internal/permissions/policy.go`, the `DenyReason` field comment: replace
`the fixed "tool call denied by user" message` with `the approver-deny reason (approverDenyReason)`; keep
the rest of the comment, and update "the legacy smart/off/prompt-all deny path" sentence so it no longer
claims that path has an empty reason (the approver-deny path now sets it).

The piped-stdin fallback (`cmd/fuse/approval.go`, `denyNonInteractive`) returns an error, not a deny,
and is unchanged.

### Verify

`go test ./internal/permissions/ ./cmd/fuse/ -run 'ApproverDenyReason|OrchestratorStandInIsPolicy|DecisionSink'`,
then `go test ./internal/permissions/`.

Commit: `Name the deciding approver in approver deny reasons`

## Task 3 - the rules-layer hint describes the rules

Tier: economy. Files: `internal/permissions/denyhint.go`, `internal/permissions/reason_bound_test.go` (append).

### Tests first

`TestDenyHint_Wording`:

- `denyHint(LayerRules)` contains `blocked by the permission rules` and does not contain `catastrophic`.
- For every layer constant in `internal/permissions/decision.go` (`LayerMediation`, `LayerDisabled`,
  `LayerModeOff`, `LayerCache`, `LayerParse`, `LayerRules`, `LayerSafelist`, `LayerHeuristic`,
  `LayerClassifier`, `LayerFetchFloor`, `LayerEditScope`, `LayerValve`, `LayerHuman`, `LayerSmartConfig`)
  plus `""`: `denyHint(l)` is non-empty, contains no em dash (U+2014), and contains
  `retrying the identical call will fail` unless `l` is one of the layers that name their own remedy
  (`LayerHeuristic`, `LayerEditScope`, `LayerFetchFloor`, `LayerValve`, `LayerMediation`).

Run and see it fail.

### Implementation (`internal/permissions/denyhint.go`)

- `LayerRules` returns
  `this call is blocked by the permission rules; retrying the identical call will fail - change the command or ask the user`.
- Replace every remaining em dash in the file (the file comment, `LayerFetchFloor`, `LayerClassifier`,
  default) with ` - `. Rewrite `deny→identical-retry` in the comment as `deny then identical-retry`.

Existing `strings.Contains(..., "retrying the identical call will fail")` checks
(`internal/permissions/decision_test.go`, `internal/agent/deny_resilience_test.go`) still hold.

### Verify

`go test ./internal/permissions/ ./internal/agent/`

Commit: `Describe rules-layer denials as permission rules, not catastrophic`

## Task 4 - the CLI adapter relays mode off

Tier: standard. Files: `cmd/fuse/cli_adapter.go`, `cmd/fuse/run.go`, `cmd/fuse/cli_adapter_env_test.go`.

### Tests first (`cmd/fuse/cli_adapter_env_test.go`)

- Update `TestCLIAdapterPassesApproverProvenance` to the new signature: `newCLIAdapter("fuse", tc.appr, false)`.
- `TestCLIAdapterPassesModeOff`:
  - `newCLIAdapter("fuse", permissions.AlwaysApprove, true).subprocessEnv(sock)` contains exactly one
    `FUSE_HITL_MODE=off`.
  - With `t.Setenv("FUSE_HITL_MODE", "off")` (an inherited stale value), `newCLIAdapter("fuse", permissions.AlwaysApprove, false).subprocessEnv(sock)`
    has no entry starting with `FUSE_HITL_MODE=` (reconcile note 3).
  - In both, `FUSE_HITL_SOCKET=<sock>` and `FUSE_HITL_PROVENANCE=policy` are present.
- `TestBuildChildAgentCLIModeFollowsGate` (same file, or a new `cmd/fuse/cli_adapter_mode_test.go`).
  Add a helper `completerOf(t, a *agent.Agent) agent.Completer` modelled on `observerOf`
  (`cmd/fuse/observe_wiring_test.go`): reflect `FieldByName("model")`, read via `reflect.NewAt(...).Elem().Interface()`.
  Build `reg := model.NewRegistry("c", map[string]model.ModelConfig{"c": {ID: "cli/claude"}})`,
  `toolReg := tools.NewRegistry()`, and `t.Setenv("HOME", t.TempDir())` for isolation. Cases:

  | cfg.Permissions.Mode | SessionMode holder | want `FUSE_HITL_MODE=off` |
  |---|---|---|
  | `off` | nil | yes |
  | `smart` | nil | no |
  | `smart` | `permissions.NewSessionMode(permissions.ModeOff)` | yes |
  | `off` | `permissions.NewSessionMode(permissions.ModeSmart)` | no (the holder wins, as for the gate) |

  For each, call `buildChildAgent(cfg, reg, "c", discardRenderer{}, "", toolReg, permissions.AlwaysApprove, nil, "child", sm, false, nil, nil)`,
  type-assert `completerOf(t, a).(*CLIAdapter)`, and check `subprocessEnv("/tmp/x.sock")`. Repeat the
  same four cases through `buildAgentCore(cfg, reg, "c", discardRenderer{}, "", nil, "root", toolReg, permissions.AlwaysApprove, sm, false, nil, nil, observe.NoopObserver{})`
  so both `newCLIAdapter` call sites are pinned (patch every cloned builder). Neither path calls
  `Complete`, so no `claude` process is spawned.

Run and see them fail (signature mismatch).

### Implementation

`cmd/fuse/cli_adapter.go`:

- Add `const hitlModeEnv = "FUSE_HITL_MODE"` with a comment: the relay's mode fact; only `off` is ever
  sent, and `fuse mcp-server` honors it only with a relay socket.
- `CLIAdapter` gains `modeOff bool`. `newCLIAdapter(fuseExe string, approve permissions.Approver, modeOff bool)`;
  extend its doc comment: `modeOff` is true when this process's gate runs at mode off
  (`--approve-all`, or a session switched to off), and it is relayed so the tool subprocess runs at the
  same posture.
- `subprocessEnv`:

```go
func (a *CLIAdapter) subprocessEnv(socketPath string) []string {
	parent := os.Environ()
	env := make([]string, 0, len(parent)+3)
	for _, e := range parent {
		// The relayed mode is this process's decision, never an inherited one.
		if strings.HasPrefix(e, hitlModeEnv+"=") {
			continue
		}
		env = append(env, e)
	}
	env = append(env,
		"FUSE_HITL_SOCKET="+socketPath,
		"FUSE_HITL_PROVENANCE="+a.approve.DecidedBy,
	)
	if a.modeOff {
		env = append(env, hitlModeEnv+"=off")
	}
	return env
}
```

  Update its doc comment to name all three variables. Update the type comment's step 3 to mention
  `FUSE_HITL_MODE` when the parent runs at mode off.

`cmd/fuse/run.go`, both call sites:

- `buildChildAgent`: `newCLIAdapter(fuseExe, approve, sessionGateMode(cfg, sm) == permissions.ModeOff)`
- `buildAgentCore`: `cliAdapter := newCLIAdapter(fuseExe, approve, sessionGateMode(cfg, sm) == permissions.ModeOff)`

Add a one-line comment at each: the same expression `buildGate` seeds the gate with, read once per
build, so a live `/mode off` reaches the subprocess at the next turn exactly as it reaches the gate.

Before committing, `grep -rn "newCLIAdapter(" cmd/` and confirm no other non-test call site exists.

### Verify

`go test ./cmd/fuse/ -run 'CLIAdapter|BuildChildAgentCLIMode'`

Commit: `Relay mode off from the parent gate to the CLI tool subprocess`

## Task 5 - the mcp-server honors the relayed mode off

Tier: standard. Files: `cmd/fuse/mcp_server.go`, `cmd/fuse/mcp_server_gate_test.go`,
`cmd/fuse/approval_provenance_binding_test.go`, new `cmd/fuse/mcp_server_relay_mode_test.go`.

### Tests first (`cmd/fuse/mcp_server_relay_mode_test.go`)

`TestRelayPermissions`: input `p := config.PermissionsConfig{Mode: "smart", AlwaysPrompt: []string{"bash:git push*"}, Auto: config.AutoConfig{Ask: []string{"bash:touch*"}}}`.

- `relayPermissions(p, "/tmp/s.sock", "off")`: `Mode == "off"`, and with `Mode` reset to `"smart"` the
  result is `reflect.DeepEqual` to `p` (nothing else changed).
- `("", "off")`, `("/tmp/s.sock", "smart")`, `("/tmp/s.sock", "garbage")`, `("/tmp/s.sock", "")`: each
  returns a value `reflect.DeepEqual` to `p`.

`TestMCPServerGateApproveAllAcrossRelay`:

- `work := t.TempDir(); t.Chdir(work)`; `cfg := config.Config{Permissions: config.PermissionsConfig{Mode: "auto", Auto: config.AutoConfig{Ask: []string{"bash:touch*"}}}}`;
  host sandbox `sandbox.NewService(sandbox.Config{Contained: false, Handler: sandbox.HandlerHost})`;
  `toolReg := defaultToolRegistry(svc, cfg.Research, nil)`.
- Socket under `os.MkdirTemp("", "hitl")` (macOS 104-byte socket path limit, as in the existing tests);
  `hitl.NewServer(sock, func(context.Context, permissions.ApprovalRequest) (bool, bool, error) { t.Error("relay approver must not be asked"); return false, false, nil })`.
- Subtest `"relayed off allows"`: `gate := buildMCPServerGate(cfg, toolReg, relayPermissions(cfg.Permissions, sock, "off"), mcpServerApprover(sock, "policy"))`;
  execute `bash` `touch ran` under a recording sink; assert exactly one decision, `allow`/`LayerModeOff`,
  no `ask`, `!res.Denied`, and `filepath.Join(work, "ran")` exists.
- Subtest `"no relayed mode denies"`: same with `relayPermissions(cfg.Permissions, sock, "")` and
  `touch ran-b`; assert one decision `deny`/`LayerRules`/`DecidedByPolicy`, `res.Denied`, `ran-b` absent.
  This proves the variable is what flipped the posture.

Run and see them fail.

### Implementation (`cmd/fuse/mcp_server.go`)

```go
// relayPermissions returns the permissions the mcp-server gate is built with.
// A parent fuse at mode off (--approve-all, or a session switched to off)
// relays FUSE_HITL_MODE=off to its cli/ model's tool subprocess, which then
// runs at the posture the parent promised. It is honored only with the
// parent's relay socket, so an editor-launched standalone mcp-server cannot be
// switched off by an environment variable alone, and only for "off"; every
// other value leaves p unchanged.
func relayPermissions(p config.PermissionsConfig, socketPath, relayMode string) config.PermissionsConfig {
	if socketPath != "" && relayMode == "off" {
		p.Mode = "off"
	}
	return p
}
```

- `buildMCPServerGate(cfg config.Config, toolReg *tools.Registry, perms config.PermissionsConfig, approve permissions.Approver)`:
  `permissions.New(perms, toolReg, approve, ...)`; the workspace root and `gateWriteRoots(cfg)` options
  still read `cfg`. Add a doc-comment sentence: `perms` is `cfg.Permissions` after `relayPermissions`;
  the config-watch reload rebuilds tools and never the gate, so the relayed mode holds for the
  subprocess lifetime (one model call).
- `runMCPServer`: read the socket once,
  `sock := os.Getenv("FUSE_HITL_SOCKET")`,
  `approve := mcpServerApprover(sock, os.Getenv("FUSE_HITL_PROVENANCE"))`,
  `gate := buildMCPServerGate(cfg, toolReg, relayPermissions(cfg.Permissions, sock, os.Getenv(hitlModeEnv)), approve)`.
  Extend the function's doc comment: with `FUSE_HITL_MODE=off` and a socket the gate runs at mode off.
- Update every existing caller to pass `cfg.Permissions` as the new third argument:
  `cmd/fuse/mcp_server_gate_test.go` (two calls) and `cmd/fuse/approval_provenance_binding_test.go`
  (three calls). `grep -rn "buildMCPServerGate(" cmd/` must show no stale call.

### Verify

`go test ./cmd/fuse/ -run 'RelayPermissions|MCPServerGate'`

Commit: `Honor relayed mode off in the mcp-server gate`

## Task 6 - hosted end-to-end proof on the loop-server wiring

Tier: standard. Depends on Tasks 1 and 3. File: `cmd/fuse/permission_unparseable_binding_test.go`.

Both new cases run on the production loop-server runtime (`harnessServer`, `runScriptedLoop`,
`scriptedBashCall`, `scriptedFinal`, `floorWorkspace`), against the scripted `recordingGateway`.

### Changes

- Add `const unparseableDenyPrefixText = "unparseable command denied; no approver bound: "` and redefine
  `unparseableProbeDenyReason = unparseableDenyPrefixText + unparseableProbe` (same value).
- Add a helper `toolMessage(gw *recordingGateway) string` returning the content of the `tool`-role
  message in the second gateway request (`gw.snapshot()[1].Messages`), `""` when absent.
- Extend the `TestLoopServerDeniesUnparseableCommandEndToEnd` case struct with `probe string` (empty
  means `unparseableProbe`) and `check func(t *testing.T, d event.PermissionDecisionPayload, tool string)`.
  When `check` is set it replaces the exact `d.Reason != tc.reason` comparison and the `modelWasTold`
  call; existing cases keep `check` nil and behave exactly as today.
- New case `"over-length command is bounded"`: yaml `"permissions:\n  mode: auto\n"`, mode `auto`,
  decidedBy `permissions.DecidedByPolicy`, `probe: "/usr/bin/touch " + strings.Repeat("a", 10000)`.
  `check` asserts: `strings.HasPrefix(d.Reason, unparseableDenyPrefixText+"/usr/bin/touch")`;
  `len(d.Reason) <= len(unparseableDenyPrefixText)+203`; `len(d.Command) <= 203`;
  `tool != ""`, `len(tool) < 1024`, `strings.Contains(tool, "…")`. The shared assertions still require
  one `deny`/`LayerRules`/mode `auto`/`DecidedByPolicy` decision. This is the hosted-visible proof that a
  refused over-length command no longer puts its full text in the durable stream or the model's context.
- New case `"credentialed URL is redacted"`: yaml
  `"permissions:\n  mode: auto\n  auto:\n    deny: [\"bash:/usr/bin/curl*\"]\n"`, mode `auto`, decidedBy `""`
  (a plain rule deny carries no provenance), `probe: "/usr/bin/curl https://u:tok@h/x"`. `check` asserts
  `d.Reason == "denied by auto-mode rules layer: /usr/bin/curl https://***@h/x"`,
  `!strings.Contains(d.Reason, "tok")`, `!strings.Contains(d.Command, "tok")`,
  `strings.Contains(tool, "***@h/x")`, `!strings.Contains(tool, "tok")`. The harness binds no sandbox
  and the command is denied before any execution, so nothing reaches the network.
- Update the test's doc comment to name the two new cases.

### Verify

`go test ./cmd/fuse/ -run 'LoopServerDeniesUnparseableCommandEndToEnd|HostedBindingDeniesUnparseableCommand' -count=1 -v`

Commit: `Prove bounded and redacted deny reasons on the loop-server wiring`

## Task 7 - documentation

Tier: economy. Files: `README.md`, `cmd/fuse/main.go`.

- `cmd/fuse/main.go:123`, the `-approve-all` help string becomes
  `"auto-approve every tool call (scripted use; equivalent to mode: off for this run, also for a cli/ model's tool subprocess - a deliberate footgun)"`.
- `README.md`, `[permissions]` section, the sentence that quotes
  ``with `unparseable command denied; no approver bound: <command>` ``: append a clause after the
  sentence's end: `The command in every deny reason is trimmed, capped at 200 characters with "…", and has URL credentials replaced with `***@`, matching the decision event's `command` field.`
- `README.md`, same section, after the paragraph ending "...`decided_by` on its `permission.decision`
  events says `policy`.", add:
  `` A policy stand-in that answers no is recorded as `tool call denied by policy approver`; a person who answers no is `tool call denied by user`. ``
  and a new short paragraph:
  `` **`--approve-all` and `cli/` models.** `--approve-all` runs the session at mode off. With a `cli/` model, approve-all is carried to the `fuse mcp-server` tool subprocess, which runs with permissions off for that call. The relay environment is `FUSE_HITL_SOCKET`, `FUSE_HITL_PROVENANCE` and `FUSE_HITL_MODE`; `fuse mcp-server` honors `FUSE_HITL_MODE` only when `FUSE_HITL_SOCKET` is also set and only for the value `off`. ``
- Write the new prose without em dashes. Do not edit unrelated existing em dashes in the README.

Verify: `go build ./cmd/fuse && go vet ./cmd/fuse/`.

Commit: `Document bounded deny reasons and approve-all across the CLI relay`

## Final gate (docket-build)

- `gofmt -l internal/ cmd/` is empty; `go vet ./...` clean.
- `go test ./...` (the full suite) green. Optionally `go test -race ./internal/permissions/ ./cmd/fuse/`.
- `git grep -n "denied by user" -- '*.go'` shows only `approverDenyReason`, its tests, and the human-deny
  expectations; `git grep -n "as catastrophic" -- '*.go'` is empty.

## Spec coverage map

| Spec section | Task |
|---|---|
| Design 1: `boundText`, `reasonCommand`, every listed site, `makePreview` untouched | 1 |
| Design 2: `approverDenyReason`, approver-deny path, `Execute` fallback, `policy.go` comment | 2 |
| Design 3: `LayerRules` hint, em dashes in `denyhint.go` | 3 |
| Design 4: `newCLIAdapter(..., modeOff)`, both `run.go` sites, `subprocessEnv` | 4 |
| Design 4: `relayPermissions`, `buildMCPServerGate` parameter, `runMCPServer` wiring | 5 |
| Design 4: README and flag text | 7 |
| ADR: none | PR body (below) |
| Testing, `internal/permissions`: `TestReasonCommand_BoundsAndRedacts`, `TestDenyReasons_UseBoundedCommand`, `TestApproverDenyReason`, `TestDenyHint_Wording`, existing exact-string tests unchanged | 1, 2, 3 |
| Testing, `cmd/fuse`: `TestOrchestratorStandInIsPolicy`, `TestCLIAdapterPassesModeOff`, `TestBuildChildAgentCLIModeFollowsGate`, `TestRelayPermissions`, `TestMCPServerGateApproveAllAcrossRelay` | 2, 4, 5 |
| Testing, hosted end to end: the two new `TestLoopServerDeniesUnparseableCommandEndToEnd` cases | 6 |
| Compatibility | PR body (below) |

## PR body notes (for the PR handoff)

- No ADR is recorded: bounding and redacting prose applies `commandPreview`'s existing rule to the
  field beside it; the approver-deny wording follows the per-gate provenance decision that `decided_by`
  is the authority; the hint is text; carrying approve-all across the relay makes the explicit-ask
  ADR's "moot, since that run is mode off" sentence true rather than changing a decision.
- Release-note items: deny reasons for commands over 200 characters or with URL userinfo change
  (truncated with `…` / `***@`; key on the prefix); policy approver denials read
  `tool call denied by policy approver`; the rules-layer hint text changes; `--approve-all` with a
  `cli/` model again approves every tool call in the subprocess (mode off); new `FUSE_HITL_MODE`
  honored by `fuse mcp-server` only with `FUSE_HITL_SOCKET` and only for `off`.
- Plan-time deviations: reconcile notes 1 to 5 above (heuristic deny unreachable, classifier reason
  included, inherited `FUSE_HITL_MODE` stripped, README placement, reflect-based completer access).
  Residual not addressed: the classifier's own rationale text is not bounded.
