<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0112 — Harness instructions: a free-text task brief composed into the system prompt](../../changes/archive/2026-11-01-0112-harness-instructions-a-free-text-task-brief-composed-into-th.md)**
<!-- docket:backlink:end -->

# Harness instructions: a free-text task brief composed into the system prompt

- Type: feat
- Priority: high

## Why

A harness spec can choose a persona (`Model.persona`, a named built-in style such as `coding` or `research`) and a per-model `system_prefix` meant for model-specific directives (`/no_think`), but it has no field for what the agent is *for*. A harness meant to be a support assistant, a release-notes writer or a code reviewer cannot say so in its own spec. Authors either misuse `system_prefix`, which belongs to one model entry and is placed *before* the persona prompt, or rely on files in a working directory, which a spec-driven or hosted harness does not have. `agent.ComposeSystemPrompt` (`internal/agent/personas.go:41`) already has the slot (`extra`), but nothing in `HarnessSpec` feeds it.

## What changes

- `HarnessSpec.instructions` (`proto/fuse/harness/v1/harness.proto`, next free field number): a string with `buf.validate` max_len 16384. UI descriptor: label "Instructions", help "What this agent is for and how it should work. Added to the system prompt after the persona.", `WIDGET_TEXTAREA`.
- Composition: the main agent's system prompt becomes persona prompt → harness instructions → existing `extra` (project/context instructions), each separated by a blank line, with `system_prefix` still first. `ComposeSystemPrompt` gains the parameter, or the instructions are folded into `extra` ahead of the existing content; either way, one ordering is documented and tested.
- Plumbed everywhere a spec becomes a running agent: `internal/harnessconfig` (spec → config, and the `e.set` export path so round-trips keep it), the YAML config schema (`instructions:` top level), `configcheck`, the loop server's spec-driven loops, and the CLI-adapter path in `cmd/fuse/run.go` (both `ComposeSystemPrompt` call sites).
- Included in the spec/settings digest so changing instructions changes the digest.
- Workers/sub-agents keep their own prompts. Instructions apply to the main agent only, and the field's help text says so.
- Docs: harness spec reference and an example spec with instructions.

## Out of scope

Per-worker instructions, templated variables in instructions, and prompt-injection filtering of instructions (they are author-controlled, like the rest of the spec).

## Acceptance criteria

- A spec with `instructions: "You are a release-notes writer."` produces a system prompt containing the persona prompt followed by that text, in the documented order (unit test on composition, plus a harnessconfig round-trip test).
- Empty instructions leave today's system prompt byte-for-byte unchanged (golden test).
- Instructions over 16384 characters fail spec validation with a field-scoped violation.
- Changing only `instructions` changes the spec digest.
- A spec-driven loop on the loop server uses the instructions (integration test with a fake/cheap gateway model; no Anthropic models for verification traffic).
