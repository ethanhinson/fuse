<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0113 — Roles: named helper definitions in the harness spec](../../changes/archive/2026-11-02-0113-roles-named-helper-definitions-in-the-harness-spec.md)**
<!-- docket:backlink:end -->

# Roles: named helper definitions in the harness spec

- Type: feat
- Priority: high

## Why

A harness spec cannot say *who* a subagent is:

- `HarnessSpec.instructions` briefs the main agent only.
- `Worker` (`proto/fuse/harness/v1/harness.proto`) carries a tool allowlist and a model alias, but no prompt and no description. It is reachable only inside a workflow subtree, where `spawn_agent` offers its bare name.
- Outside workflows, the parent model writes a fresh `system_prompt` for every child.

So delegation is unpredictable, and a spec author cannot define a reusable "reviewer" or "researcher".

A **role** is a named, reusable subagent definition. It has:

- a description the parent reads when choosing whom to delegate to;
- a brief composed into the child's system prompt;
- optionally a tool set and a model.

This change adds the data model and its validation only. The runtime uses it in a follow-up change.

## What changes

- **Proto, additive:**
  - `HarnessSpec.roles = 15`: `map<string, Role>`. Its key pattern matches `workflows` (`^[a-z0-9][a-z0-9_-]*$`, 1–64 chars). At most 32 entries.
  - `message Role`, with four fields:
    - `string description = 1`: required, min 1, max 512. The `ui` help reads "What the main agent reads when deciding whom to hand work to."
    - `string instructions = 2`: max `config.MaxInstructionsChars` (16384, the same constant as `HarnessSpec.instructions`), `WIDGET_TEXTAREA`.
    - `repeated string tools = 3`: optional.
      - Empty: the child inherits the parent's tool set. This is today's freeform-spawn posture.
      - Non-empty: the child's whole tool set, under exactly the `Worker.tools` rules. It can only narrow, the blackboard tools are always added, and the child can nest exactly when the list names `spawn_agent`.
    - `string model = 4`: an alias, max 128, `WIDGET_MODEL_ALIAS`. Empty inherits the parent's model.
  - `Workflow.roles = 4`: `repeated string`, naming the top-level roles that may take part in the workflow.
- **Config:**
  - a top-level `roles:` YAML key (`RoleConfig`) and `WorkflowConfig.Roles`;
  - `internal/harnessconfig` `FromConfig`/`toDocument` both carry roles, so spec→config→spec round-trips keep them.
- **Semantic validation** (`internal/config/validate.go`, surfaced through `ValidateHarness` violations): every `Workflow.roles` name must resolve to a defined role. An unresolved name is reported at the path `workflows.<wf>.roles[i]`.
  - Tool names and model aliases are **not** resolved at validation time. This is consistent with `Worker`: tool sets are dynamic (MCP), and `validate.go` deliberately leaves aliases unresolved. Unknown tools are refused at spawn time instead.
- **Digest:** no digest code changes. `harness.Digest` already hashes the deterministic proto encoding. `harness/digest_test.go` pins two cases:
  - an empty `roles` leaves today's digest byte-identical;
  - changing one role field changes the digest.
- **Docs:** a Roles section in `README.md`, next to the instructions section, and a two-role example in `harness/example_spec_test.go`. The proto comments explain the split: a role is who a helper is; a workflow is how a big job is divided up.

## Out of scope

- Any runtime behaviour. `spawn_agent`, prompt composition and pipelines are follow-up changes. A spec that defines roles validates and round-trips, but it runs exactly as before until then.
- Deprecating or migrating `Worker`/`workers`. They stay supported unchanged.
- Re-expressing the built-in `research` workflow's `facet-researcher` worker as a role.
- Per-role limits.
- Validating role tool names or model aliases.

## Acceptance criteria

- A spec with two roles, and a workflow listing one of them, validates. It round-trips spec→config→spec unchanged (harnessconfig test).
- Each of these fails with a field-scoped violation:
  - a missing description;
  - a description over 512 characters;
  - instructions over 16384 characters;
  - more than 32 roles;
  - a bad key;
  - a `Workflow.roles` name that is not defined, reported at `workflows.<wf>.roles[i]`.
- `harness/digest_test.go`:
  - a spec without roles has the same digest as today;
  - changing only one role's instructions changes the digest.
