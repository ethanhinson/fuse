---
id: 10
slug: shell-slash-command-ui
title: Shell Slash-Command Autocomplete + MCP & Skill Invocation
status: done
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
branch: feat/0010-shell-slash-command-ui
claimed_at: 
pr: https://github.com/ethanhinson/fuse/pull/9
blocked_by:
reconciled: false
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-08-04-shell-slash-command-ui-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-08-04-shell-slash-command-ui-design.md) |
| PR | [#9](https://github.com/ethanhinson/fuse/pull/9) |
<!-- docket:artifacts:end -->

## Why

The fuse shell has a bare slash-dispatch with no discovery surface. Users must memorize command names; MCP tools are completely invisible in the shell; skills installed mid-session require a restart to appear (a known Claude Code pain point — skills added to `~/.claude/skills` while the shell is running are silently ignored). This change makes the shell self-documenting, live-reloading, and brings MCP tools into the shell conversation.

Change 0009 (`fuse mcps` CLI + `/mcps` shell built-in) is absorbed here — the `/mcps` shell built-in is replaced by per-tool autocomplete entries, and the management CLI scope is preserved.

## What changes

- A `CommandProvider` interface — built-ins, skills, and MCP tools each implement the same source contract; a `SlashRegistry` aggregates them uniformly (Cline/Grok-Build pattern).
- **Live skill reloading** — `fsnotify` watches `~/.fuse/skills`, `~/.claude/skills`, `~/.grok/skills`; new/changed/removed skills appear in autocomplete without restarting the shell.
- **Live MCP reloading** — `fsnotify` watches `~/.fuse/config.yml`; when `fuse mcps add/remove` writes the config (from any terminal), the shell detects the change and reconnects only the delta within ~200 ms.
- A `slashCompleter` bubbletea sub-model: typing `/` opens an overlay with kind tags (`[builtin]`, `[skill]`, `[mcp:server]`) and descriptions; arrow keys navigate; Enter injects the expansion; Esc dismisses.
- MCP tools expand to a natural-language prompt template — the agent routes through the existing tool executor, no new execution path.
- `fuse mcps` top-level CLI (absorbed from 0009): `list [--live]`, `add`, `remove`, `tools`, `logs`.
- `Manager.Status()`, `Manager.Stop()`, stderr ring buffer, `internal/config/writer.go` (absorbed from 0009).

## Out of scope

- Structured argument forms for MCP tools — natural-language expansion only.
- In-shell MCP management as slash commands — `fuse mcps` CLI is the path.
- Watching config for non-MCP changes (model list, auth config, etc.).

## Open questions

None — design fully specified in the linked spec.
