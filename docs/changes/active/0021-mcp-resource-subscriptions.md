---
id: 21
slug: mcp-resource-subscriptions
title: MCP resource subscriptions — push-based updates
status: in-progress
priority: medium
type: feat
created: 2026-08-06
updated: 2026-08-08
depends_on: [19, 20]
related: [20]
discovered_from: [19]
adrs: []
spec: docs/superpowers/specs/2026-08-08-mcp-resource-subscriptions-design.md
plan:
results:
trivial: false
auto_groomable:
branch: feat/mcp-resource-subscriptions
claimed_at: 2026-08-08T19:19:14Z
pr:
blocked_by:
reconciled: false
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-08-08-mcp-resource-subscriptions-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-08-08-mcp-resource-subscriptions-design.md) |
<!-- docket:artifacts:end -->

## Why

The MCP spec 2025-03-26 formalizes **resource subscriptions** — a `subscribe`/`unsubscribe` method pair plus push-based `resource_update` notifications. This replaces the polling pattern (periodic `resources/list` or `resources/read`) with an event-driven model. For fuse, this means an MCP server (e.g. filesystem, database, project context) can push changes to the agent without the agent asking. This is especially valuable for long-running agent sessions where the workspace state changes (files created externally, git branch switches, dependency updates). Without subscriptions, the agent either polls (wasteful) or works with stale state.

This change also **builds fuse's first resource support** — fuse has none today (only
`tools/*`) — because a subscription needs a real resource to attach to.

## What changes

**Client side** (fuse subscribing to other servers' resources):

- **`resources/list` + `resources/read`** on `mcpConn` and both transports, surfaced so the agent
  can enumerate and fetch resources by URI. fuse has no resource support today; this is the
  minimal read surface the subscription attaches to.
- **`subscribe`/`unsubscribe`** on `mcpConn` — `resources/subscribe`/`resources/unsubscribe`,
  gated on `Supports("resources.subscribe")` (from change 0019).
- **Ref-counted subscription tracker** on `managedServer` — multiple call sites may subscribe to
  the same URI; re-subscribe on reconnect; not persisted across sessions.
- **`notifications/resources/updated` handler** registered on **change 0020's notification
  router** (no pump code here): mark the URI **stale** and emit `ResourceUpdatedEvent{uri}` to the
  agent tree. **Flag stale, never auto-re-read** — the agent re-reads via `resources/read` when it
  chooses. The next read of a stale URI fetches fresh.

**Server side** (dogfood — fuse's own MCP server exposes a real, mutating resource):

- **`resources/list` + `resources/read` + `resources/subscribe`** on `internal/mcp/server.go`,
  exposing **`fuse://tools`** — the live catalog of fuse's registered tools and schemas.
- **Config-watch → registry rebuild + push**: `fuse mcp-server` builds a static registry today; a
  new config watch rebuilds it (parity with the TUI's live-reload) and pushes
  `notifications/resources/updated` for `fuse://tools` to subscribed connections. This is the
  concrete mutation the whole feature is proven against — an external client (Claude Code) sees
  fuse's toolset change live.

## Out of scope

- Prompts subscriptions (`notifications/prompts/list_changed`) — same pattern, deferred.
- Auto-subscribe on first resource read — explicit subscribe only.
- Persisting subscriptions across reconnects — re-subscribe on reconnect.
- Auto-re-read on update — flag stale only.
- Server-side resources beyond `fuse://tools` (`fuse://config`, session/agent-tree state) —
  follow-ups; the standalone `fuse mcp-server` has no running session/agent tree.

## Design decisions

Design settled through an interactive brainstorm on 2026-08-08 and captured in the linked spec.
Five decisions fixed the shape: (1) build **minimal client-side `resources/list`+`read`** (fuse
has no resource support today) alongside subscribe/unsubscribe; (2) **flag stale, agent decides**
— never auto-re-read; (3) **reuse change 0020's notification router** (`depends_on: [19, 20]`),
no duplicated pump work; (4) **capability-gated, fail-open** on `Supports("resources.subscribe")`;
(5) **dogfood via fuse's own MCP server** exposing a real, mutating `fuse://tools` resource that
pushes updates on live-reload — per the human's direction to prove MCP infra through fuse's own
server rather than a test-only fixture, verified end-to-end with the real binary and TUI
screenshots.
