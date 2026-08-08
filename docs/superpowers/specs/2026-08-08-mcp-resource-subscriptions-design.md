<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0021 — MCP resource subscriptions — push-based updates](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/archive/2026-08-08-0021-mcp-resource-subscriptions.md)**
<!-- docket:backlink:end -->

# MCP resource subscriptions — push-based updates

**Change:** [#0021](../../changes/active/0021-mcp-resource-subscriptions.md) · **Status:** design · **Date:** 2026-08-08

## Problem

MCP 2025-03-26 formalizes **resource subscriptions**: a `resources/subscribe` /
`resources/unsubscribe` method pair, plus a push notification
`notifications/resources/updated` that carries **only the changed URI** (not content). This
replaces polling (`resources/list` / `resources/read` on a timer) with an event-driven model —
an MCP server pushes "resource X changed" and the client re-reads on its own schedule. Valuable
for long agent sessions where workspace state drifts.

Two facts about fuse's current state shape this change:

1. **fuse has no resource support at all today.** `internal/mcp/` implements only
   `tools/list` / `tools/call`; there is no `resources/list`, no `resources/read`, no resource
   cache, and `managedServer` (`internal/mcp/manager.go:50`) tracks only `tools`. The stub's
   "invalidate the **cached** resource" assumes infrastructure that does not exist — so this
   change must **build the minimal resource read/list surface** the subscription attaches to.
2. **The read pumps drop id-less frames.** `notifications/resources/updated` is id-less, and
   both `StdioClient.readPump` and `httpClient.readSSEPump` drop id-less frames today (learning
   `mcp-read-pumps-drop-inbound-notifications`, which names this change). **#0020 builds the
   feature-generic notification router; this change registers on it** rather than re-solving the
   pump problem — hence `depends_on: [20]`.

## Design decisions (settled)

The interactive brainstorm settled five points. `superpowers:brainstorming` was unavailable in
the grooming session, so the design was reached inline with the human (docket Skill-layer
missing-skill fallback) and is recorded here as final.

- **D1 — Build minimal client-side resource read/list.** This change adds `resources/list` and
  `resources/read` (surfaced so the agent can enumerate and fetch resources) alongside
  `subscribe`/`unsubscribe`, so a subscription has something real to attach to and re-read.
- **D2 — Flag stale, agent decides.** On `notifications/resources/updated`, mark the URI stale
  and emit a `ResourceUpdatedEvent` to the agent tree — **never auto-re-read** (avoids churn on
  a hot resource). The agent re-reads via `resources/read` when it chooses. (The stub's own
  research note recommends this.)
- **D3 — Reuse #0020's notification router.** Register a handler for
  `notifications/resources/updated` on `Manager.OnNotification(...)` (from #0020). No pump/router
  code here. `depends_on: [19, 20]`.
- **D4 — Capability-gated, fail-open.** `subscribe` is attempted only when
  `Supports("resources.subscribe")` (from #0019) is true; a non-advertising server returns an
  error on an explicit subscribe attempt, and `resources/read`/`list` on a server that advertises
  no resources simply returns empty. No hard failures (ADR-0010 posture).
- **D5 — Dogfood: fuse's own MCP server exposes a real, mutating resource.** This is **not** a
  test-only fixture. Per the human's direction ([[feedback-dogfood-fuse-mcp-server]]), fuse's own
  MCP server grows a genuine subscribable resource — **`fuse://tools`**, the live tools/skills
  catalog — that pushes `notifications/resources/updated` when fuse **live-reloads** its
  skill/MCP providers. An external client (Claude Code, Cursor) subscribed to `fuse://tools` sees
  fuse's toolset change live. This exercises the whole loop (live-reload → server push → client
  route → stale flag → re-read) against real infra.

## What we build

### Client side (fuse subscribing to other servers' resources)

1. **`resources/list` + `resources/read` on `mcpConn`** and both transports — mirror the
   existing `tools/list` / `tools/call` shape in `internal/mcp/`. Surface them so the agent can
   enumerate resources and fetch one by URI (a `read_resource` built-in tool, or an extension of
   the MCP tool surface — decided at build; the tool wiring follows
   `patch-every-cloned-child-builder`'s all-sites rule).
2. **`subscribe` / `unsubscribe` on `mcpConn`** — send `resources/subscribe` /
   `resources/unsubscribe` with `{ "uri": "…" }`. Gated on `Supports("resources.subscribe")`
   (D4).
3. **Subscription tracker on `managedServer`** — a **ref-counted** set of subscribed URIs
   (multiple call sites may subscribe to the same URI; unsubscribe on last release).
   Re-subscribe on reconnect; not persisted across sessions.
4. **`notifications/resources/updated` handler** — registered on #0020's router (D3). On a
   notification: mark the URI **stale** and emit `ResourceUpdatedEvent{server, uri}` to the agent
   tree. No auto-fetch (D2). The next `resources/read` of a stale URI fetches fresh; a URI with
   no subscription always fetches fresh (as today).

### Server side (fuse's own MCP server — the dogfood, D5)

5. **`resources/list` + `resources/read` on `internal/mcp/server.go`** — add the `resources/*`
   cases to `dispatch` (`server.go:72`). Expose **`fuse://tools`**: a JSON document listing
   fuse's currently-registered native tools and their schemas (derived from the server's
   `tools.Registry`). Minimal, real, and useful to an external assistant.
6. **`resources/subscribe` on the server** — accept a subscription to `fuse://tools`, tracked
   per-connection.
7. **Mutation source + push** — today `fuse mcp-server` builds a **static**
   `defaultToolRegistry` once (`cmd/fuse/mcp_server.go:26`) and never reloads, so `fuse://tools`
   would never change. This change adds a **config-watch → native-registry rebuild** to the
   server, bringing it to parity with the TUI's `applyConfigDiff` live-reload
   (`internal/tui/mcp_provider.go`). When the rebuild changes the tool set, the server writes a
   `notifications/resources/updated` frame for `fuse://tools` (id-less, via the server's frame
   encoder at `server.go:67`) to every subscribed connection. This is the concrete, real
   mutation the whole feature is proven against.

## Out of scope

- **Prompts subscriptions** (`notifications/prompts/list_changed`) — same pattern, deferred.
- **Auto-subscribe on first read** — explicit `subscribe` only.
- **Persisting subscriptions across reconnects** — re-subscribe on reconnect.
- **Auto-re-read on update** — flag stale only (D2).
- **Server-side resources beyond `fuse://tools`** — one real resource proves the loop; more
  (`fuse://config`, session state) are follow-ups. Session/agent-tree resources are notably
  out — the standalone `fuse mcp-server` process has no running session/agent tree.
- **A generic file-watch resource** (`fuse://config`) — considered; deferred (adds an fsnotify
  mechanism `fuse://tools`'s config-diff already covers for the tool catalog).

## Tests

- **`resources/list` / `read` (client)**: against a server double advertising resources, list and
  read a URI; a server advertising none returns empty (fail-open, D4).
- **Subscribe gating (D4)**: `Supports("resources.subscribe")` true ⇒ subscribe sends;
  false ⇒ explicit subscribe returns an error, read/list still work.
- **Ref-counted tracker**: two subscribes + one unsubscribe keeps the URI subscribed; the second
  unsubscribe releases it; reconnect re-subscribes tracked URIs.
- **Update handler (D2/D3)**: an id-less `notifications/resources/updated` routed through #0020's
  router marks the URI stale and emits `ResourceUpdatedEvent`; **no** automatic `resources/read`
  fires; the next explicit read fetches fresh.
- **Server `fuse://tools` (D5)**: `resources/list` on fuse's server includes `fuse://tools`;
  `resources/read` returns the current tool catalog JSON.
- **Server push (D5)**: after a config-watch rebuild changes the tool set, subscribed connections
  receive a well-formed id-less `notifications/resources/updated` for `fuse://tools`.
- **Dogfood end-to-end (real binary + TUI screenshots)** — the human's stated verification: a
  real fuse client subscribes to a real `fuse mcp-server`'s `fuse://tools`; a live-reload on the
  server pushes the update; the client routes it and the TUI renders the stale/updated indicator.
  Verified against the real `cmd/fuse` seam (learnings `mcp-read-pumps-drop-inbound-notifications`,
  `verify-tool-loop-at-gateway-seam`) and captured via `teatest` final-frame screenshots
  (`teatest-final-frame-via-finalmodel-view`).

## Risks & mitigations

- **Pump drops the notification** — foreclosed by reusing #0020's router (D3); the end-to-end
  real-binary test is the backstop.
- **Read churn on a hot resource** — foreclosed by flag-stale-not-auto-read (D2).
- **`fuse://tools` never actually mutates** — the config-watch → rebuild (item 7) is the explicit
  mutation source; a test asserts a rebuild produces a push. Without it the dogfood is inert, so
  it is in scope, not optional.
- **Stale subscription after reconnect** — re-subscribe tracked URIs on reconnect; server
  subscriptions are per-connection.
- **Capability mismatch** — fail-open per ADR-0010.

## Dependencies & follow-ups

- **`depends_on: [19, 20]`** — #0019 supplies `Supports("resources.subscribe")` + `notify`;
  **#0020 supplies the notification router this registers on.**
- Follow-ups: prompts subscriptions; `fuse://config` (with a file watch); richer server-side
  resources (session state) once the server carries session context.
