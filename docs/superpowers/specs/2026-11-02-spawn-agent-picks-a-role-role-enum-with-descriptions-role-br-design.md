<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0114 — spawn_agent picks a role: role enum with descriptions, role brief composed into the child prompt](../../changes/active/0114-spawn-agent-picks-a-role-role-enum-with-descriptions-role-br.md)**
<!-- docket:backlink:end -->

# spawn_agent picks a role: role enum with descriptions, role brief composed into the child prompt

- Type: feat
- Priority: high

## Why

Roles can now be defined in a spec, but the runtime ignores them. The parent model still invents each child's system prompt. Outside workflows, it cannot choose a typed helper at all.

Delegation should become *selection*: the parent picks a role by its description, and the runtime builds the child from the role's definition.

## What changes

- **`spawn_agent` schema** (`internal/tools/spawn_agent.go`):
  - When the spec defines roles, the tool gains an optional `role` string parameter. It has an `enum` of role names, and its description lists one `name — description` line per offered role, because JSON Schema has no per-value text.
  - Outside a workflow subtree, every role is offered. Inside one, the workflow's `roles` are offered, or every role when the workflow lists none.
  - The parameter sits beside the existing `worker` enum, which is unchanged. With no roles defined, the schema and description are byte-identical to today's.
- **System prompt sentence:** when roles exist, one sentence ("Prefer a named role when one fits the task.") is appended *after* `spawnAgentBlock` as a separate string. `spawnAgentBlock` (`cmd/fuse/run.go:76`) is a `const` that `withoutUnavailableSpawnBlock` (`cmd/fuse/prompt_blocks.go`) strips by exact substring, so the block itself must not change. Roles are not listed a second time.
- **Resolution in `workflowScope`** (`cmd/fuse/workflow.go`):
  - `checkRequest` learns roles. An unknown role is refused before any node or quota is spent, with the list of valid names. So are `role` with `system_prompt` ("a role supplies the system prompt; drop system_prompt or the role") and `role` with `worker`.
  - The `act == nil` branch accepts a role (unlike `worker`, which needs a workflow), because roles are global.
  - `prepareChild`/`childPlan` gain the role's instructions and model.
  - A `resolveRoleTools` mirrors `resolveWorkerTools`:
    - a non-empty `role.tools` becomes the child's whole tool set, and the call's `tools` may only narrow it (widening is refused);
    - an empty `role.tools` inherits the parent's tool set.
  - **Blackboard:** a non-empty `role.tools` sets `plan.bound = true`, so `loopWiring.childTools` (`cmd/fuse/loop_wiring.go`) wires the blackboard tools exactly as for a worker. An empty one leaves `bound = false`, which is the unchanged inherit path.
  - **Model precedence:** the call's `model` wins over `role.model`, which wins over the parent's model (as for workers). The persona follows the resolved model (`mc.Persona`), so a role that pins a model may change the child's persona. This is intended.
- **Prompt slot:** each child-builder site already passes `""` as the instructions argument (`/* subagents never get the harness instructions */`, for example `runtime_binding.go:164`, `:593`). For a role spawn it passes `role.instructions` instead. The child's prompt is then persona → role brief → child extra, the same slot `HarnessSpec.instructions` takes for the root. The harness instructions still never reach a subagent.
  - Refusing `role` together with `system_prompt` upstream is required, because a set `opts.SystemPrompt` takes the `buildChildAgent` override branch.
- **Parity across all five child-builder sites:**
  - `cmd/fuse/runtime_binding.go`: the sites commented "1 of 4", "2 of 4" and "3 of 4";
  - `cmd/fuse/loop_server.go` ("4 of 4");
  - `cmd/fuse/shell.go:333`, whose stale "2 of 3" comment is corrected.

  One table-driven test covers all five.
- **Observability:** these additive fields name the role:
  - `Role` on `agent.AgentNode` and its snapshot (`internal/agent/tree.go`);
  - `Role` on `event.SpawnStartPayload` (`internal/event/event.go`), which is additive JSON on a wire contract that consumers read;
  - a `role` attribute on the spawn observe span (`internal/agent/spawn.go`, beside `worker`).
- **Limits:** role spawns count against the `Agents` brakes and workflow pools exactly as other spawns do. There are no per-role limits.

## Out of scope

- Pipeline steps naming a role (follow-up change).
- Per-role limits, a strict "must use a role" mode, and a per-role prompt-override flag.
- Any change to `worker` semantics or to the built-in `research` workflow.

## Acceptance criteria

- With roles `reviewer` (tools `[read_file, grep]`) and `writer` (no tools):
  - a `role: reviewer` spawn builds a child whose system prompt is persona → reviewer instructions → child extra (reusing the byte-for-byte composition pin test from harness instructions). Its tool set is exactly read_file, grep and the blackboard tools;
  - `role: writer` inherits the parent's tools, without the blackboard tools.
- The `role` enum and description lines appear outside workflows. Inside a workflow, only that workflow's listed roles appear.
- Each of these is refused in `checkRequest` with a tool error, before any node or quota is spent:
  - `role` with `system_prompt`;
  - `role` with `worker`;
  - an unknown role;
  - a `tools` list that widens a role.
- The table-driven parity test passes on all five child-builder sites.
- With no roles defined, these are byte-identical to today's (existing goldens):
  - the `spawn_agent` schema;
  - the system prompts;
  - the `SpawnStartPayload` JSON (the role field is omitted when empty).
- The role name appears on the child's tree node, snapshot, `spawn.start` event and span.
- One live check against a cheap gateway model (not Anthropic) shows the parent choosing a role by its description on a matching task.
