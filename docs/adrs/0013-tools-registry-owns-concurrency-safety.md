---
id: 13
slug: tools-registry-owns-concurrency-safety
title: tools.Registry is the home for concurrency safety (config-watch live reload)
status: Accepted
date: 2026-08-08
supersedes: []
reverses: []
relates_to: []
change: 21
---

## Context

Change 0021 (MCP resource subscriptions) added a config-watch goroutine to `fuse
mcp-server` (`cmd/fuse/mcp_server.go`) that rebuilds the native tool registry in
place on config change — parity with the TUI's live-reload — so that the dogfood
`fuse://tools` resource actually mutates and pushes
`notifications/resources/updated`.

But `internal/tools/Registry` was a plain struct (an `order []` slice plus a
`byName` map) with **no synchronization**, while the MCP server's `Serve` loop
concurrently dispatches `tools/list` (`Schemas`), `tools/call` (`Has` +
`Execute`), and `resources/read` (`toolCatalogJSON` → `Schemas`) against that same
registry. A concurrent map read + write can panic and crash the entire server
process. A whole-branch code review caught this as a blocker: the `-race` suite
was green only because no test drove a tool request concurrently with a reload,
so the hazard was latent, not absent.

## Decision

Put the concurrency invariant in `tools.Registry` itself. Add a `sync.RWMutex`
guarding `order`/`byName`:

- **RLock** in the read methods — `Has`, `Schemas`, `Tools`, `Subset`, `Clone`,
  and the lookup in `Execute`. The lock is **released before the tool body runs**,
  so a long-running tool call never serializes the registry.
- **Lock** in the write methods — `Register`, `Unregister`.

Rejected alternatives:

- **A mutex confined to the MCP server** around registry access — rejected
  because `Registry` is a shared type used in multiple places, and locating the
  invariant at a single caller would force every other current and future caller
  to re-derive it.
- **A pointer-swap of an immutable registry on reload** — rejected because the
  permissions gate holds the same `*Registry` pointer and must observe the
  change; an in-place reconcile preserves that, a pointer swap would strand the
  gate on a stale registry.

Live-reload makes concurrent access a **permanent property** of the registry, not
a one-off, so the invariant belongs at the type where every caller inherits it.

## Consequences

- **Enables** safe live config reload of the standalone MCP server: the
  `fuse://tools` resource can mutate under concurrent tool dispatch without
  risking a process-crashing map race.
- **Costs** a small lock on every registry read. The `Execute` path holds the
  lock only for the lookup, not for the tool run, so hot tool calls are not
  serialized.
- **The invariant now lives at the type**, so every future concurrent caller
  inherits safety without re-deriving it.
- A `-race` regression test (`TestRegistryConcurrentReadWrite` in
  `internal/tools/registry_test.go`) pins the property.
