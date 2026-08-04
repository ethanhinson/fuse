---
id: 3
slug: hitl-permissions-mcp
title: HITL Permission Layer + MCP Client Integration
status: proposed
priority: high
type: feat
created: 2026-08-03
updated: 2026-08-03
depends_on: [2]
related: [1, 2]
discovered_from: []
adrs: []
spec: docs/superpowers/specs/0003-hitl-permissions-mcp.md
plan:
results:
trivial: false
auto_groomable: false
branch:
pr:
blocked_by:
reconciled: false
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [0003-hitl-permissions-mcp.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/0003-hitl-permissions-mcp.md) |
<!-- docket:artifacts:end -->

## Why

Every tool the agent calls — `bash`, `write_file`, `edit_file` — fires immediately without user awareness. There is no way to intercept a destructive command before it runs. MCP (Model Context Protocol) support is equally absent: other agents can connect to MCP servers (filesystem, GitHub, databases) to extend their tool surface, but Fuse cannot. These two gaps are tightly coupled: the right time to add MCP is when the permission layer exists, so MCP tools get HITL treatment from day one rather than being bolted on later as a trusted-by-default afterthought.

## What changes

- New `internal/permissions/` package: `ToolPolicy` state machine, 3-source policy merge (safe-list → config patterns → session cache), `PermissionGate` wrapping the tool registry.
- User approval prompt integrated into the bubbletea TUI (change 0002): inline amber block with `[y]es / [s]ession / [n]o` keybindings; "allow for session" caches the decision in-memory for the process lifetime.
- New `internal/mcp/` package: stdio transport MCP client (JSON-RPC 2.0 over stdin/stdout), tool discovery on session start, `MCPTool` adapter implementing `tools.Tool` and registering into the global registry under `mcp:<server>/<tool>`.
- `config.yml` gains `permissions:` block (mode, patterns) and `mcp_servers:` block (name, command, env).
- Agent loop wired through `PermissionGate.Execute()` instead of direct registry calls.

## Out of scope

- HTTP/SSE MCP transport (stdio only in this change).
- MCP resource and prompt endpoints.
- Persistent approval history across sessions.
- Approval UI in one-shot (`fuse "task"`) mode — one-shot always auto-approves; no human available.

## Open questions

None — design is settled in the spec.

## Reconcile log

<!-- Appended by docket-implement-next's reconcile pass. -->
