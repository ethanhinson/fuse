# Permission deny reasons bound and redact the command they echo; denials and hints say what denied; approve-all carries across the CLI-model relay

Change 0098 — Permission deny reasons bound and redact the command they echo

Designed against main at eb16b8a (per-gate provenance and the unparseable-command deny both merged). Line numbers are for that commit.

## One change, not two

Build this as one change carried by 0098. Kill 0097 as a duplicate: its two items (approve-all across the relay, "denied by user" wording) are folded in below. The three wording items (deny reason, approver-deny reason, rules-layer hint) live in the same two files and are asserted by the same exact-string tests, so splitting them means two PRs rewriting the same expectations; the relay fix is about twenty lines and would not stand as a PR of its own. Update 0098's title to the one above and set `related: [94, 96, 97]`.

## Problem

1. Deny reasons echo the full command, unbounded and unredacted. Every reason that names a command is built by plain concatenation: the smart-mode floor deny and ask (`internal/permissions/gate.go:602`, `:607`), the unparseable deny in smart and auto mode (`:632`, `:803`), the auto rules deny (`:797`, `:813`) and the heuristic deny (`:838`). The same gate already bounds and redacts the structured `Command` field of the decision event through `commandPreview` (`gate.go:1134-1144`: URL userinfo replaced with `***@` by `redactURLUserinfo`, `:1152`, then a 200-character cap), with a comment that says decision events are a logged, exported audit record. The prose `Reason` sits beside it on the same event (`emitDecision`, `:483-497`) and goes to the model in the tool result (`Execute`, `:457-471`), unbounded. A refused over-length command (`maxCommandBytes`, `internal/permissions/shellparse.go:20`, is 10000) puts a 10KB reason into a durable event and a tool result on every attempt; a `curl https://user:token@host/...` that a rule or the egress heuristic denies repeats the token in prose next to a `Command` field that hid it.

2. A policy deny claims a person. `Execute` falls back to `tool call denied by user` when a deny carries no reason (`gate.go:457`), and the approver-deny path emits that same string as the decision reason (`:663`) while returning a `ToolPolicy` with no `DenyReason` (`:664`). The orchestrator's deny stand-in (`cmd/fuse/orchestrate_cli.go`, `orchestrateCLIApprover`) is policy provenance since the per-gate provenance change, so its denials now read `tool call denied by user` with `decided_by: policy`. The structured field is right; the sentence is wrong.

3. The rules-layer hint calls every rules deny catastrophic. `denyHint(LayerRules)` (`internal/permissions/denyhint.go:9-10`) says "blocked by auto-mode policy as catastrophic". That hint is appended to every `LayerRules` deny (`gate.go:469`): explicit-ask denies, smart-mode denies, unparseable denies and plain `auto.deny` rule hits, none of which is catastrophic, and the smart-mode ones are not auto mode.

4. `--approve-all` stops at the CLI-model relay. The flag sets the parent's mode to off (`cmd/fuse/main.go:174-176`) and binds `Policy(autoApprove)` (`cmd/fuse/approval.go:38-39`). A `cli/` model runs its tools in a `fuse mcp-server` subprocess (`cmd/fuse/cli_adapter.go:57-66`) that loads its own config from disk (`main.go:51`) and builds its gate at that config's mode (`cmd/fuse/mcp_server.go:52-55`, `buildMCPServerGate`). Since the relay carries provenance (`subprocessEnv`, `cli_adapter.go:110-115`; `mcpServerApprover`, `mcp_server.go:78-88`), that gate is a policy gate, so an `auto.ask`, `always_prompt`, smart-mode catastrophic or unparseable match inside the subprocess is denied at the rules layer. The parent promised to approve every tool call; the subprocess refuses some. The explicit-ask ADR calls the approve-all approver "moot, since that run is mode off"; across the relay it is not.

## Design

### 1. One bounded, redacted command text for every reason

- `gate.go`: factor the body of `commandPreview` into `boundText(s string) string` (trim, `redactURLUserinfo`, cap at 200 characters with the existing `…`). `commandPreview` calls it; add `reasonCommand(command string) string { return boundText(command) }` with a comment saying every reason that echoes a command goes through it, so prose and the structured preview can never disagree about what the record may contain.
- Replace `+ command` with `+ reasonCommand(command)` at `gate.go:602`, `:607`, `:632`, `:797`, `:803`, `:813`, `:838`. The explicit-ask reasons name a pattern, not a command, and are untouched. `policyForcedDeny` keys on prefixes and is unaffected.
- `makePreview` (`:1157`, the 80-character TUI prompt line) keeps its own cap; it is shown to a person, not recorded. Out of scope.
- Reason text for a command under 200 characters with no URL userinfo is byte-identical to today, so every existing exact-string assertion on short commands still holds.

### 2. The approver deny names its decider

- `gate.go`: add `approverDenyReason(decidedBy string) string`: `DecidedByPolicy` returns `tool call denied by policy approver`; anything else returns `tool call denied by user`.
- The approver-deny path (`:663-664`) emits `approverDenyReason(g.approvalProvenance)` and returns `ToolPolicy{Enabled: true, AutoApprove: false, DenyReason: <same string>, DenyLayer: LayerHuman}`, so `Execute` no longer needs its fallback for this path. `Execute` (`:457`) keeps a fallback for a reason-less deny and uses `approverDenyReason(g.approvalProvenance)` there too.
- `decided_by` stays the authority; the sentence now agrees with it. The piped-stdin fallback returns an error rather than a deny (`approval.go`, `denyNonInteractive`) and is unchanged.
- `internal/permissions/policy.go:58-62` comment: replace "the fixed 'tool call denied by user' message" with "the approver-deny reason (`approverDenyReason`)".

### 3. The rules-layer hint describes the rules

- `denyhint.go:9-10`: `LayerRules` returns `this call is blocked by the permission rules; retrying the identical call will fail - change the command or ask the user`. The reason sentence before it already says which rule (floor, ask pattern, unparseable, `auto.deny`), so the hint does not repeat it.
- While in the file, replace the em dash in the other hint strings and the file comment with ` - `. Tests match hints with `strings.Contains` on `retrying the identical call will fail` (`internal/permissions/decision_test.go:168`, `internal/agent/deny_resilience_test.go:32`), which survives.

### 4. Approve-all crosses the relay

- `cmd/fuse/run.go`: `newCLIAdapter(fuseExe string, approve permissions.Approver, modeOff bool)`; both call sites (`:454`, `:990`) pass `sessionGateMode(cfg, sm) == permissions.ModeOff`, the same expression the gate is built with at `:416`. The shell rebuilds its agent per turn, so a live `/mode off` is picked up at the next turn like the gate's own mode; a mid-turn switch is not propagated, exactly as the subprocess gate is built once per call.
- `cli_adapter.go`: `CLIAdapter` gains `modeOff bool`; `subprocessEnv` appends `FUSE_HITL_MODE=off` when it is set, and nothing otherwise. The variable is named for the relay on purpose: it is a relay fact, not a general mode override.
- `cmd/fuse/mcp_server.go`: add `relayPermissions(p config.PermissionsConfig, socketPath, relayMode string) config.PermissionsConfig`: when `socketPath != ""` and `relayMode == "off"`, return `p` with `Mode = "off"`; otherwise return `p` unchanged. `runMCPServer` passes `relayPermissions(cfg.Permissions, os.Getenv("FUSE_HITL_SOCKET"), os.Getenv("FUSE_HITL_MODE"))` into `buildMCPServerGate` (give `buildMCPServerGate` a `config.PermissionsConfig` parameter in place of reading `cfg.Permissions` itself; its other two options still read `cfg`). The socket requirement means an editor-launched standalone `fuse mcp-server` cannot be switched off by an environment variable alone; only a parent `fuse` relay, which already chose mode off for itself, can. The config-watch reload (`mcp_server.go:196-208`) rebuilds tools and never the gate, so the relayed mode holds for the subprocess lifetime, which is one model call.
- The subprocess then records `allow` at `LayerModeOff` for every call, the same posture the parent runs. Its approver stays whatever `mcpServerApprover` chose; mode off asks nothing, so it is never consulted.
- README: the `--approve-all` flag text and the mcp-server section each get one sentence: with a `cli/` model, approve-all is carried to the tool subprocess, which runs with permissions off for that call; the relay env is `FUSE_HITL_SOCKET`, `FUSE_HITL_PROVENANCE`, `FUSE_HITL_MODE`.

### ADR

None. Bounding and redacting prose to match the structured preview applies an existing rule (`commandPreview`'s comment) to the field beside it; the approver-deny wording follows the per-gate provenance ADR's own statement that `decided_by` is the authority; the hint is text; and carrying approve-all across the relay makes that ADR's "moot, since that run is mode off" sentence true rather than changing a decision. Note in the PR that no ADR is recorded and why.

## Testing

`internal/permissions`:

- `TestReasonCommand_BoundsAndRedacts`: `reasonCommand("curl https://u:p@h/x")` is `curl https://***@h/x`; a 10001-byte command yields exactly 200 characters plus `…`; `"  ls  "` yields `ls`; `reasonCommand(x) == commandPreview("bash", bashArgs(x))` for each row, pinning that the two cannot drift.
- `TestDenyReasons_UseBoundedCommand`, mode auto, `AlwaysApprove`, stub registry, table of (config, command, expected prefix): unparseable over-length (`sudo ` plus 10000 `a`), `auto.deny: ["bash:curl*"]` with `curl https://u:tok@h/x`, heuristic egress deny with `curl https://u:tok@evil/x`, and mode smart `rm -rf /` with 5000 bytes of trailing arguments under policy provenance (floor deny). Each decision's `Reason` starts with the expected prefix, has length at most `len(prefix) + 200 + len("…")`, does not contain `tok`, and contains `***@` where a URL was present; `res.Output` has the same bound before the `; ` hint. The human-bound smart floor ask (`catastrophicAskPrefix`) is checked the same way through a recorder.
- `TestApproverDenyReason`: a smart-mode gate with `Human(deny)` executing `make build` records `deny`/`LayerHuman`/`DecidedByHuman` with reason `tool call denied by user` and `res.Output` starting with it; the same with `Policy(deny)` records `tool call denied by policy approver`, `DecidedByPolicy`, and `res.DenyLayer == LayerHuman`.
- `TestDenyHint_Wording`: `denyHint(LayerRules)` contains `blocked by the permission rules` and not `catastrophic`; for every `Layer*` constant, `denyHint` contains no em dash and contains `retrying the identical call will fail` or names its own remedy.
- Existing exact-string tests (`unparseable_test.go`, `patternparity_test.go`, `patterns_test.go`, `decision_test.go`) pass unchanged: their commands are short and carry no userinfo.

`cmd/fuse`:

- `TestOrchestratorStandInIsPolicy` (`approval_provenance_binding_test.go`) updates its expected reason to `tool call denied by policy approver`.
- `TestCLIAdapterPassesModeOff`: `newCLIAdapter("fuse", permissions.AlwaysApprove, true).subprocessEnv(sock)` contains `FUSE_HITL_MODE=off`; with `false` no entry starts with `FUSE_HITL_MODE=`; socket and provenance entries are unchanged in both.
- `TestBuildChildAgentCLIModeFollowsGate`: `buildChildAgent` with `cfg.Permissions.Mode = "off"` and a `cli/` model yields an adapter whose `subprocessEnv` carries `FUSE_HITL_MODE=off`; with mode `smart` it does not; with mode `smart` and a `SessionMode` holder set to off it does (the same seam the gate uses).
- `TestRelayPermissions`: (socket, `off`) sets `Mode = "off"` and changes nothing else; (no socket, `off`), (socket, `smart`), (socket, `garbage`), (socket, empty) all return the input unchanged.
- `TestMCPServerGateApproveAllAcrossRelay`: a real `hitl.NewServer` whose function fails the test if called; `buildMCPServerGate(cfg, toolReg, relayPermissions(cfg.Permissions, sock, "off"), mcpServerApprover(sock, "policy"))` with `auto.ask: ["bash:touch*"]`, chdir into a temp workspace, execute `touch ran`: one `allow` at `LayerModeOff`, no `ask`, the file exists. The same gate built with `relayPermissions(cfg.Permissions, sock, "")` denies at `LayerRules` and the file is absent, so the test proves the env is what flipped it.
- `TestLoopServerDeniesUnparseableCommandEndToEnd` (`permission_unparseable_binding_test.go`) gains a case, end to end on the production loop-server wiring: the scripted bash call is `/usr/bin/touch ` plus 10000 `a` (over the parser cap, so refused), mode auto, no rules. Assert one `deny`/`LayerRules`/`DecidedByPolicy` decision whose `Reason` starts with `unparseable command denied; no approver bound: /usr/bin/touch` and has length at most `len(unparseableDenyPrefix) + 203`, that the `permission.decision` event's `Command` field is at most 203 bytes, and that the `tool`-role message handed to the model on the next turn is under 1KB and contains `…`. This is the hosted-visible proof that a refused over-length command no longer puts its full text in the durable stream or the model's context. A second new case uses `/usr/bin/curl https://u:tok@h/x` under `auto.deny: ["bash:/usr/bin/curl*"]` and asserts the reason contains `***@h/x` and not `tok`, and that the tool message does not contain `tok` either.

## Compatibility

- Deny reasons for commands over 200 characters, or containing URL userinfo, change: they are truncated with `…` or carry `***@`. Reasons for short, credential-free commands are byte-identical. Consumers that keyed on an exact reason for a long command should key on the prefix.
- Policy-decided approver denials (today only the orchestrator's deny stand-in) now read `tool call denied by policy approver` instead of `tool call denied by user`; `decided_by` is unchanged and remains the field to key on. Human denials are unchanged.
- The rules-layer hint text changes; the other hints lose their em dash. Nothing parses hints.
- `--approve-all` with a `cli/` model once again approves every tool call in the subprocess, now by running it at mode off (before the provenance change the relay auto-approved prompts; between that change and this one, rule matches were denied). A new environment variable, `FUSE_HITL_MODE`, is honored by `fuse mcp-server` only when `FUSE_HITL_SOCKET` is also set and only for the value `off`. No config key changes. Call the approve-all restoration and the new variable out in the release note.
