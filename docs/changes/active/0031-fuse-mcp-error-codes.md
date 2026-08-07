---
id: 31
slug: mcp-error-codes
title: Adopt MCP-specific JSON-RPC error code range
status: in-progress
priority: low
type: chore
created: 2026-08-06
updated: 2026-08-07
depends_on: [3]
related: [19]
discovered_from: [18]
adrs: []
spec:
plan:
results:
trivial: true
auto_groomable:
branch: feat/mcp-error-codes
pr:
blocked_by:
reconciled: true
claimed_at: 2026-08-07T01:44:30Z
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
<!-- docket:artifacts:end -->

## Why

The MCP spec v2025-03-26 reserves the JSON-RPC error code range `-32900` to `-32999` for MCP-specific errors (tool not found, resource not found, parse error, etc.), distinct from standard JSON-RPC errors (`-32700`, `-32600`–`-32603`). The current fuse MCP implementation uses only standard JSON-RPC error codes. Adopting the MCP range improves interoperability: clients like Claude Code and Cursor can distinguish "tool not found" from "invalid params" and surface better diagnostics.

## What changes

- Define MCP error code constants in `internal/mcp/` (a new `errors.go`): `ErrToolNotFound = -32900`, `ErrResourceNotFound = -32901`, etc. matching the spec range, with a small helper to test membership in the MCP range.
- Server (`internal/mcp/server.go`, not `mcp_server.go`): return `-32900` (ToolNotFound) from `handleCall` when the requested tool name is not in the registry, instead of letting it fall through the gate to a generic `isError` tool result (`registry.go:83`).
- Client (`internal/mcp/client.go`): the read pump at `call()` (`client.go:217-219`) currently discards `resp.Error.Code`, surfacing only the message. Preserve the code so MCP-specific codes from downstream servers are recognizable to callers (e.g. via a typed error carrying the code).
- Very small change — one new constants file plus a handful of lines in `server.go`/`client.go`.

## Research notes

Spec reference: MCP v2025-03-26, Error Codes section. The range is `-32900` to `-32999`. Common codes: `-32900` (ToolNotFound), `-32901` (ResourceNotFound), `-32902` (PromptNotFound), `-32903` (ListResultEmpty), `-32904` (ConnectionClosed). Implementations that don't know the range will treat these as generic application errors (`-32000`), which works but loses diagnostic value.

## Reconcile log

### 2026-08-07 — reconcile before build

Verified against current code (`internal/mcp/`). Findings folded into **What changes**:

- The change body named `mcp_server.go`; the actual file is `internal/mcp/server.go`. Server-side error emission lives in `errResp` and today uses `-32601` (method not found, `server.go:85`) and `-32600` (invalid params, `server.go:110`).
- "Tool not found" is not currently a JSON-RPC error at all: `handleCall` hands every tool name to `gate.Execute → registry.Execute`, which returns an `isError: true` tool *result* (`registry.go:83`) for an unknown tool. Emitting `-32900` requires an explicit registry-membership check in `handleCall` before dispatch.
- The client discards the error code: `StdioClient.call` wraps only `resp.Error.Message` (`client.go:217-219`). Surfacing MCP-specific codes means preserving `resp.Error.Code` through a typed error.
- Resource-not-found (`-32901`) has no call site yet — fuse's MCP server exposes only `tools/*`, no `resources/*` methods (`server.go` dispatch). Constants are still defined for completeness, but only `-32900` gets a live server call site this change. Noted in scope.

Scope unchanged and still trivial. Two adjacent observations reported (not filed — auto-capture is disabled this repo): (1) `server.go:110` uses `-32600` ("Invalid Request") for an invalid-params condition where standard JSON-RPC prescribes `-32602`; (2) `initialize` advertises `protocolVersion "2024-11-05"` while the error range is a v2025-03-26 feature — protocol-version negotiation is change #19's job and does not gate adopting the codes.
