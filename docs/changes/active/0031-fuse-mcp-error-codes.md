---
id: 31
slug: mcp-error-codes
title: Adopt MCP-specific JSON-RPC error code range
status: proposed
priority: low
type: chore
created: 2026-08-06
updated: 2026-08-06
depends_on: [3]
related: [19]
discovered_from: [18]
adrs: []
spec:
plan:
results:
trivial: true
auto_groomable:
branch:
pr:
blocked_by:
reconciled: false
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
<!-- docket:artifacts:end -->

## Why

The MCP spec v2025-03-26 reserves the JSON-RPC error code range `-32900` to `-32999` for MCP-specific errors (tool not found, resource not found, parse error, etc.), distinct from standard JSON-RPC errors (`-32700`, `-32600`–`-32603`). The current fuse MCP implementation uses only standard JSON-RPC error codes. Adopting the MCP range improves interoperability: clients like Claude Code and Cursor can distinguish "tool not found" from "invalid params" and surface better diagnostics.

## What changes

- Define MCP error code constants in `internal/mcp/`: `ErrToolNotFound = -32900`, `ErrResourceNotFound = -32901`, etc. matching the spec range.
- Update the MCP server (`mcp_server.go`) to return MCP-specific codes where applicable (tool not found, resource not found).
- Update the MCP client's error mapping to recognize and surface MCP-specific codes from servers.
- Very small change — one file, ~20 lines.

## Research notes

Spec reference: MCP v2025-03-26, Error Codes section. The range is `-32900` to `-32999`. Common codes: `-32900` (ToolNotFound), `-32901` (ResourceNotFound), `-32902` (PromptNotFound), `-32903` (ListResultEmpty), `-32904` (ConnectionClosed). Implementations that don't know the range will treat these as generic application errors (`-32000`), which works but loses diagnostic value.
