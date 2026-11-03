---
id: 117
slug: 'cancel-rpc-on-the-networked-loop-server'
title: 'Cancel RPC on the networked loop server'
status: 'in-progress'
priority: 'high'
type: 'feat'
deliverable: 'pr'
created: '2026-11-03'
updated: '2026-11-03'
depends_on: []
stacked_on:
related: [47, 49, 51, 53, 54, 55, 103, 106, 108]
discovered_from: []
adrs: [78]
spec: 'docs/superpowers/specs/2026-11-03-cancel-rpc-on-the-networked-loop-server-design.md'
plan: 'docs/superpowers/plans/2026-11-03-cancel-rpc-on-the-networked-loop-server.md'
results:
auto_groomable:
hold:
branch_prefix:
branch: 'feat/cancel-rpc-on-the-networked-loop-server'
pr:
blocked_by:
reconciled: true
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-11-03-cancel-rpc-on-the-networked-loop-server-design.md](../../superpowers/specs/2026-11-03-cancel-rpc-on-the-networked-loop-server-design.md) |
| Plan | [2026-11-03-cancel-rpc-on-the-networked-loop-server.md](../../superpowers/plans/2026-11-03-cancel-rpc-on-the-networked-loop-server.md) |
| ADRs | [ADR-0078](../../adrs/0078-loop-cancellation-is-a-terminal-stream-event-plus-an-in-memo.md) |
<!-- docket:artifacts:end -->

## Why

The loop API can start, steer and watch a loop but never stop one. A loop a caller no longer wants runs until it finishes on its own or, if interactive, until the idle timeout fires, holding a live-loop slot, a sandbox pool, an MCP manager and model tokens the whole time. Callers that count or bill live loops cannot release them, and a loop stuck in a long model call or tool cannot be interrupted by anyone except the operator draining the server.

## What changes

A `CancelLoop` RPC, guarded by a new `loop:cancel` scope (also satisfied by `loop:admin`), stops a loop the caller owns in its own tenant: the in-flight model call, tool batch, sandbox process and child agents unwind through context cancellation, the loop's pools and MCP manager are released, and a terminal `loop.cancelled` event carrying the caller's reason is appended as the last event of the stream and delivered to every observer. The registry flips to not-live before the call returns so the tenant's capacity is freed at once; a cancelled loop is sealed: Send refuses it with a distinct "cancelled" precondition instead of silently resuming it. Cancel is idempotent and tenant-isolated (cross-tenant is NotFound), works for loops owned by another replica by relaying through the durable stream, and is counted by a `fuse_loops_cancelled_total` metric projected from the event.

## Out of scope

Cancelling individual children, tools or turns (steer-with-interrupt already covers that); an orchestrator tool that cancels its target; a `loop.cancel` method on the stdio JSON-RPC binding; terminal events for idle reaps and drains (those loops stay resumable and are not cancelled); registry-level cancelled state or listing cancelled loops; a bounded reason taxonomy as a metric label; dashboards or alerts for the new counter; any host-application specifics.

## Reconcile log

### 2026-11-03

Unchanged against `origin/main` 7ea0da7. Spec anchors (loop.proto RPCs, loopauth scopes, InProc ErrLoopFinished/resume path, idle TTL, relay.go) hold. Change 0118 is building in parallel and also edits loop.proto (SpecPolicy.carried_skills_allowed); rebase onto origin/main before PR and regenerate protos if it lands first.
