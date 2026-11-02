<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0115 — Pipeline steps name a role; roles in the synthesis palette](../../changes/active/0115-pipeline-steps-name-a-role-roles-in-the-synthesis-palette.md)**
<!-- docket:backlink:end -->

# Pipeline steps name a role; roles in the synthesis palette

- Type: feat
- Priority: low

## Why

Pipeline steps (`internal/pipeline`) can name a workflow `worker` (`stepSpec.Worker`), but not a role. Once `spawn_agent` can pick a role, roles are the main way to say who does a piece of work. A pipeline the model writes for itself should be able to assign a step to the `researcher` or `reviewer` role the spec defines, and pipeline synthesis should know those roles exist.

## What changes

- Pipeline step spec (`internal/pipeline/parse.go`): an optional `role` field beside `worker`. Naming both is a parse error, as is an unknown role.
- Engine: a role step builds its child through the same role resolution as `spawn_agent`: the prompt slot, tools narrow-only, and the model precedence.
- `refuseUnboundWorkerSteps` (`cmd/fuse/run.go`) keeps refusing `worker` steps outside a workflow. It does **not** refuse `role` steps there, because roles are global.
- Synthesis: the role palette (`name — description`) joins `pipelineSynthPalette` (`cmd/fuse/run.go`), so a synthesized plan can assign steps to roles.
- The `pipeline_run` tool description mentions the `role` field when roles exist. Without roles, it is unchanged.

## Out of scope

- Any change to how pipelines are scheduled or limited.
- Making roles mandatory in pipelines.

## Acceptance criteria

- A pipeline with a `role: reviewer` step runs that step with the reviewer's composed prompt and tool set (engine test with a fake model).
- These are parse errors: a step naming both `role` and `worker`; an unknown role.
- Synthesis input includes the role palette when roles exist. Without roles, the synthesis prompt and the `pipeline_run` description are byte-identical to today's.
