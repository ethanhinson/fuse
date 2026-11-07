---
id: 137
slug: 'tool-execution-seam-with-in-process-and-remote-worker-execut'
title: 'Tool execution seam with in-process and remote-worker executors'
status: 'killed'
priority: 'medium'
type: 'feat'
deliverable: 'pr'
created: '2026-11-03'
updated: '2026-11-07'
depends_on: [128, 129, 130, 133]
stacked_on:
related: [75, 117]
discovered_from: []
adrs: []
spec: 'docs/superpowers/specs/2026-11-03-tool-execution-seam-with-in-process-and-remote-worker-execut-design.md'
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
| Spec | [2026-11-03-tool-execution-seam-with-in-process-and-remote-worker-execut-design.md](../../superpowers/specs/2026-11-03-tool-execution-seam-with-in-process-and-remote-worker-execut-design.md) |
<!-- docket:artifacts:end -->

## Why

Every tool runs in the loop's own process: `Tool.Execute` is a direct call, MCP tools hold their connection pointer, and `bash` reaches its sandbox through a per-loop pool. Callers that want heavy or untrusted tool work on separate compute nodes have no seam to do it, and the loop process carries every sandbox and connection itself.

## What changes

A `ToolExecutor` interface in front of the registry with the in-process executor (today) and a durable-queue executor: tool calls become claimable jobs on the shared store, a `fuse tool-worker` process claims them, runs the tool against the loop's workspace and artifact store, and returns the result through the store; the loop awaits it with the same timeout and cancel semantics. Conformance suite for executors; a loop + worker two-process test.

## Out of scope

Remote MCP execution (MCP tools stay in-process in this change; the executor routes them locally); remote child agents; scheduling policy beyond FIFO per tenant.

## Why killed

Out of scope after the 2026-11-06 refocus: fuse is a coding harness run as one container per task, with no shared multi-tenant server or harness specs. Only needed for the shared server or fleet.
