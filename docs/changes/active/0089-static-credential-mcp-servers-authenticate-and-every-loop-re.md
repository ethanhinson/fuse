---
id: 89
slug: 'static-credential-mcp-servers-authenticate-and-every-loop-re'
title: 'Static-credential MCP servers authenticate, and every loop reports its MCP servers'
status: 'proposed'
priority: 'medium'
type: 'fix'
deliverable: 'pr'
created: '2026-10-31'
updated: '2026-10-31'
depends_on: []
stacked_on:
related: [85, 86]
discovered_from: []
adrs: [61, 62]
spec: 'docs/superpowers/specs/2026-10-31-static-credential-mcp-servers-authenticate-and-every-loop-re-design.md'
plan:
results:
auto_groomable:
hold:
branch_prefix:
branch:
pr:
blocked_by:
reconciled: false
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-10-31-static-credential-mcp-servers-authenticate-and-every-loop-re-design.md](../../superpowers/specs/2026-10-31-static-credential-mcp-servers-authenticate-and-every-loop-re-design.md) |
| ADRs | [ADR-0061](../../adrs/0061-a-loop-runs-a-client-harnessspec-layered-over-the-operator-c.md), [ADR-0062](../../adrs/0062-getcapabilities-advertises-from-the-enforcement-code-and-car.md) |
<!-- docket:artifacts:end -->

## Why

An MCP server with auth type `static` is accepted by validation, layering and capability discovery, but `GetAccessToken` has no `static` case, so connecting fails and the server is skipped with only a host log line. Any MCP server that fails to connect is skipped silently, and a client cannot see which servers connected or which tools they contributed.

## What changes

- `static` auth sends the resolved secret as a bearer credential, like `bearer`.
- A golden test dials a test MCP server once per advertised spec MCP auth kind, so every advertised kind is backed by enforcement.
- New durable event `mcp.server`, one per configured server at loop start, right after `loop.harness`: name, transport, auth type, connected, error and tool names.
- Skip-and-continue behavior is kept.

## Out of scope

- A `required` flag that fails the loop when a server is down.
- Per-loop tool names in GetCapabilities.
- New auth kinds.
