# Tool-less loops, roles and workers

Change 0116 — Tool-less loops, roles and workers

- Type: feat
- Priority: high

## Why

A caller can narrow what a loop's model may call, but cannot say "nothing". Today the only levers are `Permissions.disabled`, a list of exact tool names, and `PermissionMode` (`off`, `prompt-all`, `smart`, `auto`), which decides how calls are gated, not which exist. `PermissionGate.Schemas()` (`internal/permissions/gate.go`) drops disabled names from what the model sees, but it filters a registry that was already composed: `defaultToolRegistry` (`cmd/fuse/run.go`) registers `DefaultTools` (which includes `segment_read`) and `CodeindexTools`, and `rootTools` (`cmd/fuse/loop_wiring.go`) adds `spawn_agent`, the five `blackboard_*` tools and `pipeline_run`. A spec that disables all eleven platform tools therefore still offers eight fuse-internal tools on the first model call: `segment_read`, `codeindex_impact`, `codeindex_callers`, `blackboard_write`, `blackboard_read`, `blackboard_wait`, `blackboard_keys` and `blackboard_delete`.

None of the eight is needed on a loop that cannot call tools:

- `segment_read` recovers raw transcript regions the summarizer archived. It reads the segments directory `tools.SetSegmentsDir` installs, which only the interactive shell sets (`cmd/fuse/shell.go`). The loop server builds root and child agents with a nil `SegmentSink` (`cmd/fuse/loop_server.go`), so on a server loop the tool always answers "no segments". History recovery there is the Tier-2 anchored summary the summarizer injects in place of the compacted region; that is a model call, not a tool, and keeps working without `segment_read`.
- `codeindex_impact` and `codeindex_callers` shell out to a `codeindex` binary on the host and are a repository convenience, not a correctness dependency.
- The blackboard carries results between agents in one tree. A loop that cannot spawn has no second agent to talk to, and a child returns its answer through the spawn result (`lastAssistantText`), not the blackboard.

The same gap exists one level down. `Role.tools` and `Worker.tools` mean "inherit the parent's set" when empty and "exactly these, plus the blackboard" when not (`resolveRoleTools`, `wireChildBlackboard`), so a helper that should only think and write, such as a drafter, summarizer, classifier or judge, cannot be expressed; its narrowest form is one real tool, which widens its blast radius and invites the model to use a tool it was never meant to.

Callers need "no tools" to mean exactly that: a model call with no `tools` key, for the loop and for a child.

## What changes

### 1. The shape: `no_tools` on `HarnessSpec`, `Role` and `Worker`

One `bool no_tools` field, same name and meaning in all three messages, mirrored in config as `no_tools` on `Config`, `RoleConfig` and `WorkerConfig` (`internal/config/schema.go`) with both `harnessconfig` directions (`ToConfig`, `FromConfig`) mapping it.

Why a spec field and not the alternatives:

- `Permissions.disabled: ["*"]` would turn a list of exact names into a pattern language, hide a mode switch inside a deny list, and still answer nothing about the internal tools, because `disabled` filters after composition and is also the gate's defense in depth at `Execute`. It would also make `disabled` mean two different things in operator YAML and in a spec.
- A `PermissionMode` value would conflate composition with gating. Mode governs calls that exist, can be flipped at run time through `SessionMode`, and is validated as an enum a caller presents as "how to ask". A tool-less loop makes no gate decisions at all.
- A field reads as what it is: part of the loop's composition, carried by the spec, covered by its digest, and the same word for the loop, a role and a worker.

The three states, documented in the proto comments:

- `HarnessSpec`: **all** (default: the platform tools, spec MCP tools and internal tools, minus `permissions.disabled`) or **none** (`no_tools: true`).
- `Role`: **inherit** (`tools` empty, `no_tools` false), **narrowed** (`tools` named: the whole set, plus the blackboard), **none** (`no_tools: true`).
- `Worker`: **named** (`tools`, plus the blackboard) or **none** (`no_tools: true`); empty `tools` without `no_tools` stays invalid.

### 2. Which internal tools a tool-less loop or child still gets: none

- A tool-less loop's registry is empty. In the loop server's `NewToolRegistry` (`cmd/fuse/loop_server.go`) a tool-less spec yields `tools.NewRegistry()` with no MCP manager attached (spec MCP servers are not started), and `rootTools` is skipped, so there is no `spawn_agent`, no blackboard and no `pipeline_run`. The CLI path (`fuse run` with `no_tools: true` in config, `cmd/fuse/run.go`) goes through the same helper, so the two entry points cannot drift. The permission gate wraps the empty registry unchanged; `Schemas()` returns nothing and the adapter's `tools,omitempty` (`internal/model/adapter.go`) leaves the key off the wire.
- A tool-less child's registry is empty too, blackboard included. `childPlan` gains a `none` state; `childTools` (`cmd/fuse/loop_wiring.go`) returns `tools.NewRegistry()` for it and skips the spawn, blackboard and pipeline wiring. "Bound children always get the blackboard" stays true for narrowed roles and named workers; a tool-less child is not bound to a set, it has none. Every cloned child-builder site (the role-parity rows in `cmd/fuse/role_parity_test.go`) builds it the same way, and pipeline steps that name a tool-less role or worker inherit this through `sp.Spawn`.
- Narrow-only holds in both directions. A tool-less role or worker under a tool-ful parent narrows to nothing; a spawn-time `tools` request on it is refused with `role %q permits no tools` (worker likewise) unless the request is empty, and a `model` override is still honoured. A role with tools under a tool-less loop is unreachable, because the loop is never offered `spawn_agent`; it stays valid, like any other field the model cannot use.
- The one exception is `return_result`. A tool-less role or worker child whose spawn or pipeline step sets `expects` is offered exactly the synthesized `return_result` tool (`withReturnResultTool`, `internal/agent/loop.go`) and nothing else. `return_result` is the structured-result channel, not a capability tool: it is added per run outside the registry, so the child's registry stays empty and it still gets no blackboard, `spawn_agent` or `pipeline_run`. A tool-less loop has no `expects`, so it never sees it.
- The persona preamble (`internal/agent/personas.go`) tells every persona it "has access to file and shell tools". A tool-less loop or child gets a one-line variant stating it has no tools and answers in plain text; the caller's `instructions` and a role's brief are unchanged. The persona golden test is updated.

### 3. Validation

Semantic rules in `internal/config/validate.go`, surfaced through `harness.Validate` and `ValidateHarness` with the existing paths:

- `roles.<name>.no_tools` with a non-empty `tools`: "names tools and no_tools; choose one".
- `workflows.<wf>.workers.<w>.no_tools` with a non-empty `tools`: the same message.
- `Worker.tools` loses its `repeated.min_items: 1` shape rule; the semantic rule becomes "must name at least one tool or set no_tools". `harness/testdata/shape_cases.json` (shared with the TS SDK's shape test) and the Go shape cases are updated together.
- `HarnessSpec.no_tools` rejects nothing else. `permissions.disabled`, `mcp_servers`, `skills`, `roles`, `workflows`, `pipeline` and the `agents` limits stay valid beside it and are simply unreachable, matching how `permissions.disabled` treats a disabled `skill` or `spawn_agent` today.

### 4. Digest, capabilities, events

- The digest (`harness.Digest`, deterministic proto encoding) covers the field with no code change. `harness/digest_test.go` pins, as `TestDigestCoversRoles` does, that an unset flag leaves an existing digest (`rolelessDigest`) unchanged and that flipping `no_tools` on the spec, a role or a worker changes it.
- `GetCapabilitiesResponse` gains `repeated string spec_features`, mirrored as `Capabilities.SpecFeatures` (`internal/capabilities`), listing `no_tools`. An older server drops an unknown field and would run the loop with every tool, which is the opposite of what the caller asked for, so a client must be able to check before `StartLoop`. The Go and TS SDKs expose the list.
- The `loop.harness` record already holds the spec as sent, so a tool-less run is attributable from its events.

### 5. Tests

- Config validator: the two conflict rules; a worker with `no_tools` and no `tools` is valid; a worker with neither is still refused, now by the semantic rule and path.
- Shape parity: the updated worker case passes in Go and in the TS SDK from the one shared file.
- Digest: coverage and pinning, as above.
- Spawn parity (`cmd/fuse/role_parity_test.go` and the worker equivalent): a tool-less role child and a tool-less worker child are offered zero tools at every cloned child-builder site; a spawn naming `tools` on one is refused; a `model` override on one still applies; a narrowed role beside it still gets its tools plus the blackboard.
- Pipeline: a step naming a tool-less role runs its child with zero tools. A tool-less step that sets `expects` is offered exactly `[return_result]` at the gateway.
- Real loop server (`cmd/fuse/loop_harness_test.go`, through `harnessServer` and the recording gateway): a spec with `no_tools: true`, `max_turns: 1` and a spec MCP server makes one model call whose request has no `tools` key, whose system prompt carries the tool-less persona line, and starts no MCP server; the same server runs an ordinary spec with its full tool set. A second case runs the CLI config path with `no_tools: true` and asserts the same first call.
- Capabilities: `GetCapabilities` lists `no_tools` in `spec_features`.
- Persona golden: the tool-less preamble.

## Out of scope

- Globs, wildcards or a deny-all in `permissions.disabled`.
- Changing what an empty `Role.tools` means; it stays inherit.
- Removing `segment_read` and the codeindex tools from the loop server's default registry for ordinary loops. They are dead weight there today, but that is a separate cleanup with its own blast radius.
- Per-loop control over individual internal tools.
- Any caller UI.

## Acceptance criteria

- A spec with `no_tools: true` started on the real loop server makes its first model call with no `tools` key on the wire: not the platform tools, not spec MCP tools, and none of `segment_read`, `codeindex_impact`, `codeindex_callers` or the five `blackboard_*` tools. No MCP server named in that spec is started.
- The same holds for `fuse run` with `no_tools: true` in config.
- A spec without `no_tools` keeps its digest, and the first model call of an ordinary spec on the same server is byte-identical to today's.
- A role or worker with `no_tools: true` spawns a child offered zero tools, including no blackboard tools, at every cloned child-builder site and from a pipeline step, except that a spawn or step setting `expects` is offered exactly `return_result`. A spawn-time `tools` request on it is refused with a message naming the role or worker; a `model` override is honoured.
- A narrowed role and a named worker still receive their tools plus the five blackboard tools, unchanged.
- `harness.Validate` and `ValidateHarness` report `roles["x"].no_tools` or `workflows["wf"].workers["w"].no_tools` when `no_tools` is set beside a non-empty `tools`; a worker with neither `tools` nor `no_tools` is refused on `...workers["w"].tools`; a worker with only `no_tools` is valid. The TS SDK's shape test passes from the same `shape_cases.json`.
- `HarnessSpec.no_tools` beside `permissions.disabled`, `mcp_servers`, `skills`, `roles`, `workflows` or `pipeline` validates.
- The digest changes when `no_tools` flips on the spec, a role or a worker.
- `GetCapabilities` lists `no_tools` under `spec_features`, and both SDKs expose it.
- A tool-less loop's or child's system prompt does not claim access to file or shell tools.
- The proto comments on `HarnessSpec.no_tools`, `Role.no_tools`, `Role.tools`, `Worker.no_tools` and `Worker.tools` state the inherit, narrowed and none states exactly as above.
