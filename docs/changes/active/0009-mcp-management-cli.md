---
id: 9
slug: mcp-management-cli
title: "`fuse mcps` MCP Server Management CLI + `/mcps` Shell Built-in"
status: proposed
priority: medium
type: feat
created: 2026-08-04
updated: 2026-08-04
depends_on: [7]
related: [3, 7, 8]
discovered_from: []
adrs: []
spec: docs/superpowers/specs/0009-mcp-management-cli.md
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
| Spec | [0009-mcp-management-cli.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/0009-mcp-management-cli.md) |
<!-- docket:artifacts:end -->

## Why

There is currently no way to inspect, add, or remove MCP servers without hand-editing `~/.fuse/config.yml` and no visibility into whether configured servers are actually connected or what tools they registered. This makes MCP a black box — you configure it and hope it worked.

## What changes

- `fuse mcps` CLI subcommand with six operations: `list` (static + `--live`), `add`, `remove`, `tools`, `logs`, `debug`.
- `Manager.Status() []ServerStatus` — live connection/tool/log snapshot from the running manager.
- Per-server stderr ring buffer (200 lines) for stdio servers — feeds `fuse mcps logs`.
- `internal/config/writer.go` — additive YAML surgery for `add`/`remove` without clobbering other config keys.
- `/mcps` slash built-in in the interactive shell, wired to the already-running Manager (no extra dials); supports `list`, `logs NAME`, `tools`, `debug`, and forwarding `add`/`remove` to the config writer with a restart advisory.

## Out of scope

- Hot-reload of the MCP manager without restarting the shell.
- Log streaming / tail-f.
- In-place field editing (remove + add is the workflow).
