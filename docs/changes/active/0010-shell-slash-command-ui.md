---
id: 10
slug: shell-slash-command-ui
title: Shell Slash-Command Autocomplete + MCP & Skill Invocation
status: proposed
priority: medium
type: feat
created: 2026-08-04
updated: 2026-08-04
depends_on: []
related: [7, 8, 9]
discovered_from: []
adrs: []
spec: docs/superpowers/specs/2026-08-04-shell-slash-command-ui-design.md
plan:
results:
trivial: false
auto_groomable: false
branch:
claimed_at:
pr:
blocked_by:
reconciled: false
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-08-04-shell-slash-command-ui-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-08-04-shell-slash-command-ui-design.md) |
<!-- docket:artifacts:end -->

## Why

The fuse shell has a bare slash-dispatch with no discovery surface. Users must memorize command names; MCP tools are completely invisible in the shell even though the MCP client stack is fully operational. This change makes the shell self-documenting and brings MCP tools into the shell conversation without new execution machinery.

Change 0009 (`fuse mcps` CLI + `/mcps` shell built-in) is absorbed here — its scope is expanded to include the full autocomplete UI, and the `/mcps` shell built-in is replaced by the richer per-tool entries in the autocomplete list.

## What changes

- A `SlashRegistry` that aggregates built-in commands, loaded skills, and MCP tools from the running manager into one filterable list.
- A `slashCompleter` bubbletea sub-model: typing `/` opens an overlay with kind tags (`[builtin]`, `[skill]`, `[mcp:server]`) and descriptions; arrow keys navigate; Enter selects and injects the expansion; Esc dismisses.
- MCP tools expand to a natural-language prompt template ("Use the `echo` tool from the `everything` MCP server…") — the agent routes through the existing tool executor, no new execution path.
- `fuse mcps` top-level CLI (absorbed from 0009): `list [--live]`, `add`, `remove`, `tools`, `logs`.
- `Manager.Status() []ServerStatus` + stderr ring buffer on stdio servers (absorbed from 0009).
- `internal/config/writer.go` for additive YAML writes (`AddMCPServer` / `RemoveMCPServer`).

## Out of scope

- Structured argument forms for MCP tools — natural-language expansion only.
- In-shell MCP management (`/mcps` list/add/remove) — that is the `fuse mcps` CLI.
- New MCP servers or transport types — only what is already connected at startup appears in the list.

## Open questions

None — design fully specified in the linked spec.
