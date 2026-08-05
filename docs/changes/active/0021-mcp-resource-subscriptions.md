---
id: 21
slug: mcp-resource-subscriptions
title: MCP resource subscriptions — push-based updates
status: proposed
priority: medium
type: feat
created: 2026-08-06
updated: 2026-08-06
depends_on: [19]
related: [20]
discovered_from: [19]
adrs: []
spec:
plan:
results:
trivial: false
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

The MCP spec 2025-03-26 formalizes **resource subscriptions** — a `subscribe`/`unsubscribe` method pair plus push-based `resource_update` notifications. This replaces the polling pattern (periodic `resources/list` or `resources/read`) with an event-driven model. For fuse, this means an MCP server (e.g. filesystem, database, project context) can push changes to the agent without the agent asking. This is especially valuable for long-running agent sessions where the workspace state changes (files created externally, git branch switches, dependency updates). Without subscriptions, the agent either polls (wasteful) or works with stale state.

## What changes

- **`subscribe`/`unsubscribe` methods** on `mcpConn` (and implementations): sends `{"method":"resources/subscribe","params":{"uri":"..."}}` and `{"method":"resources/unsubscribe","params":{"uri":"..."}}`.
- **Resource update notification handler** in the MCP read pump: when a `resource_update` notification arrives (no ID, method `notifications/resources/updated`), the manager invalidates the cached resource and emits an event to the agent tree (`ResourceUpdatedEvent{uri}`).
- **Subscription manager** on `managedServer`: tracks which resource URIs are currently subscribed, with a reference count (multiple tools may subscribe to the same URI).
- **Integration with `resources/list` and `resources/read`**: when a cached resource is invalidated by a push notification, the next read re-fetches. Without a subscription, reads always fetch fresh (same as today).
- **Config surface**: a `subscriptions` capability gate (requires change 0019) — servers that don't advertise `resources.subscribe` return an error on subscribe attempts.

## Out of scope

- Prompts subscriptions (`notifications/prompts/list_changed`) — same pattern, deferred to a follow-up.
- Auto-subscribe on first resource read — explicit subscribe only.
- Persisting subscriptions across reconnects — re-subscribe on reconnect.

## Research notes (input for the brainstorm)

The resource subscription pattern mirrors the WebSocket/EventSource model: the client subscribes, the server pushes `resource_update` notifications when the resource changes, and the client can read the updated resource with `resources/read`. The notification carries only the URI — the content is not inlined (client must read). The subscription is per-connection; on reconnect, the client must re-subscribe. Key design decision: should the agent loop automatically re-read a subscribed resource on update notification, or just flag it as stale? The former is more useful but could cause churn; the latter is safer and lets the agent decide when to re-read. Fuse should start with the safer approach: flag stale, let the agent decide.
