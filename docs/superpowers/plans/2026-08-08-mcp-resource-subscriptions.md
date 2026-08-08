<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0021 — MCP resource subscriptions — push-based updates](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0021-mcp-resource-subscriptions.md)**
<!-- docket:backlink:end -->

# MCP Resource Subscriptions Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

> **Plan authored by docket-implement-next (plan-role auto-fallback).** The configured plan skill
> `superpowers:writing-plans` was not invocable on this machine (Skill tool returned "Unknown
> skill"), so per docket's Skill-layer missing-skill rule the implementer authored this plan
> directly in the writing-plans format. This degradation is also noted in the PR body.

**Goal:** Add MCP resource subscriptions to fuse — a minimal client-side `resources/list`/`read` + `subscribe`/`unsubscribe` surface that flags a URI stale on push, and a dogfood server-side `fuse://tools` resource that pushes `notifications/resources/updated` when fuse live-reloads its tool registry.

**Architecture:** Client side mirrors the existing `tools/*` request shape on `mcpConn` and registers a `notifications/resources/updated` handler on change 0020's notification router (`Manager.OnNotification`), emitting a `ResourceUpdatedEvent` via an observer fan-out modeled on `ProgressEvent`/`OnProgress`. A ref-counted per-server subscription tracker re-subscribes on reconnect. Server side adds `resources/*` cases to `Server.dispatch`, exposes `fuse://tools` from the tool registry, tracks per-connection subscriptions, and a config-watch (parity with the TUI's `applyConfigDiff`) rebuilds the registry and pushes an id-less update frame. The TUI surfaces a stale/updated indicator.

**Tech Stack:** Go, JSON-RPC over stdio/HTTP/streamable-HTTP, bubbletea/lipgloss TUI, fsnotify, teatest for TUI e2e + final-frame screenshots, `charmbracelet/freeze` (optional) for PNG capture.

## Global Constraints

- **Fail-open (ADR-0010 / D4):** capability mismatches and empty resource sets never hard-fail — `subscribe` on a non-advertising server returns an error only on an *explicit* attempt; `resources/read`/`list` on a server with no resources returns empty.
- **Flag stale, never auto-re-read (D2):** on `notifications/resources/updated`, mark the URI stale and emit an event; never issue an automatic `resources/read`.
- **Reuse change 0020's router (D3):** register on `Manager.OnNotification("notifications/resources/updated", …)`. Do NOT touch the read pumps — they already route id-less frames (confirmed at reconcile; learning `mcp-read-pumps-drop-inbound-notifications` is foreclosed by #0020).
- **All-sites rule (`patch-every-cloned-child-builder`):** any client tool-surface wiring in `cmd/fuse` must be applied at every cloned builder site — enumerate by grep at implementation time, never from this list.
- **Verify at the gateway seam (`verify-tool-loop-at-gateway-seam`):** end-to-end proof drives the real `cmd/fuse` binary; the teatest harness fakes the Completer seam but the MCP client/tool path is teatest-reachable.
- **Screenshot capture (`teatest-final-frame-via-finalmodel-view`):** capture from `FinalModel().View()` (not `FinalOutput`) and force `termenv.TrueColor` around the render; write to `FUSE_SCREENSHOT_DIR`, freeze best-effort.
- **Content-block rendering (`mcp-render-all-content-block-types`):** `resources/read` results carry typed content/resource blocks — render all types, never collect only `type==text`.

---

## File Structure

- `internal/mcp/resources.go` (new) — client-side resource types, `resources/list`/`read` calls on the Manager/mcpConn, `ResourceUpdatedEvent`, `ResourceObserver`, `OnResource`, the `notifications/resources/updated` handler, stale-flag state.
- `internal/mcp/subscriptions.go` (new) — the ref-counted per-`managedServer` subscription tracker; `subscribe`/`unsubscribe`; re-subscribe-on-reconnect hook.
- `internal/mcp/manager.go` (modify) — `managedServer` gains resource/subscription fields; register the resource handler alongside progress/stream; wire re-subscribe into reconnect.
- `internal/mcp/server.go` (modify) — `resources/list`/`resources/read`/`resources/subscribe` cases in `dispatch`; `fuse://tools` resource from the registry; per-connection subscription set; push helper using `Server.encode`.
- `cmd/fuse/mcp_server.go` (modify) — config-watch → registry rebuild → push (parity with `internal/tui/mcp_provider.go applyConfigDiff`).
- `internal/tui/` (modify) — surface the stale/updated indicator for a subscribed resource.
- Tests colocated: `internal/mcp/resources_test.go`, `internal/mcp/subscriptions_test.go`, `internal/mcp/server_resources_test.go`, `cmd/fuse/mcp_server_resources_test.go` (or the existing gateway-seam e2e harness), `internal/tui/…_test.go` (teatest + screenshot).

---

## Task 1: Client resource types + `resources/list` / `resources/read`

**Files:**
- Create: `internal/mcp/resources.go`
- Test: `internal/mcp/resources_test.go`

**Interfaces:**
- Consumes: `mcpConn.call(ctx, method, params)` (conn.go); `managedServer`/`Manager` (manager.go).
- Produces: `type Resource struct { URI, Name, MIMEType string }`; `func (m *Manager) ListResources(ctx, server string) ([]Resource, error)`; `func (m *Manager) ReadResource(ctx, server, uri string) (ResourceContents, error)` where `ResourceContents` carries the typed content blocks.

- [ ] **Step 1: Write failing test** — against an in-package server double advertising resources: `ListResources` returns them; a server advertising none returns empty (no error). `ReadResource` returns the content blocks for a URI.
- [ ] **Step 2: Run test, verify it fails.** `go test ./internal/mcp/ -run TestListReadResources -v`
- [ ] **Step 3: Implement** the calls mirroring the `tools/list`/`tools/call` shape (`c.call(ctx, "resources/list", nil)` / `"resources/read", {uri}`), decoding all content-block types.
- [ ] **Step 4: Run tests, verify pass.**
- [ ] **Step 5: Commit.**

## Task 2: Ref-counted subscription tracker + subscribe/unsubscribe (capability-gated)

**Files:**
- Create: `internal/mcp/subscriptions.go`
- Modify: `internal/mcp/manager.go` (`managedServer` fields)
- Test: `internal/mcp/subscriptions_test.go`

**Interfaces:**
- Consumes: `ServerCapabilities.Supports("resources.subscribe")` (capabilities.go); `mcpConn.call`.
- Produces: `func (m *Manager) Subscribe(ctx, server, uri string) error`; `func (m *Manager) Unsubscribe(ctx, server, uri string) error`; internal ref-count map on `managedServer`; `func (ms *managedServer) resubscribeAll(ctx) error`.

- [ ] **Step 1: Write failing tests** — (a) `Supports` true ⇒ subscribe sends `resources/subscribe`; false ⇒ explicit subscribe returns an error, list/read still work (fail-open, D4). (b) two `Subscribe` + one `Unsubscribe` keeps the URI subscribed; second `Unsubscribe` releases it. (c) reconnect re-subscribes tracked URIs.
- [ ] **Step 2: Run tests, verify they fail.**
- [ ] **Step 3: Implement** ref-counted tracker + gated calls + resubscribe hook.
- [ ] **Step 4: Run tests, verify pass.**
- [ ] **Step 5: Commit.**

## Task 3: `notifications/resources/updated` handler → stale flag + `ResourceUpdatedEvent` (D2/D3)

**Files:**
- Modify: `internal/mcp/resources.go`, `internal/mcp/manager.go` (register handler)
- Test: `internal/mcp/resources_test.go`

**Interfaces:**
- Consumes: `Manager.OnNotification(method, NotificationHandler)` (notification_router.go).
- Produces: `type ResourceUpdatedEvent struct { Server, URI string }`; `type ResourceObserver func(ResourceUpdatedEvent)`; `func (m *Manager) OnResource(obs ResourceObserver)`; `func (m *Manager) handleResourceUpdated(server string, params json.RawMessage)`; a stale-URI set queried on next read.

- [ ] **Step 1: Write failing test** — an id-less `notifications/resources/updated` routed through the router marks the URI stale and fans a `ResourceUpdatedEvent`; **no** automatic `resources/read` fires; the next explicit read fetches fresh (stale cleared). Mirror the `handleProgress`/`OnProgress` fan-out with a copied-slice under mutex.
- [ ] **Step 2: Run test, verify it fails.**
- [ ] **Step 3: Implement** handler + observer fan-out + register in `manager.go` next to `progressNotifyMethod`/`streamNotifyMethod`.
- [ ] **Step 4: Run tests, verify pass.**
- [ ] **Step 5: Commit.**

## Task 4: Server `resources/list` + `resources/read` exposing `fuse://tools` (D5)

**Files:**
- Modify: `internal/mcp/server.go`
- Test: `internal/mcp/server_resources_test.go`

**Interfaces:**
- Consumes: `Server.reg` (tool registry, `.Schemas()`); `Server.dispatch` switch; `Server.encode`.
- Produces: `resources/list` returns `[{uri: "fuse://tools", …}]`; `resources/read` of `fuse://tools` returns the current tool-catalog JSON as a resource content block.

- [ ] **Step 1: Write failing test** — `resources/list` on the server includes `fuse://tools`; `resources/read` returns the current tool catalog JSON derived from the registry.
- [ ] **Step 2: Run test, verify it fails.**
- [ ] **Step 3: Implement** the two `dispatch` cases + a `fuse://tools` renderer over the registry. Advertise `resources` (and `resources.subscribe`) in the `initialize` capabilities.
- [ ] **Step 4: Run tests, verify pass.**
- [ ] **Step 5: Commit.**

## Task 5: Server `resources/subscribe` (per-connection) + push helper

**Files:**
- Modify: `internal/mcp/server.go`
- Test: `internal/mcp/server_resources_test.go`

**Interfaces:**
- Produces: `resources/subscribe` records the URI on the connection; `func (s *Server) pushResourceUpdated(uri string)` encodes an id-less `notifications/resources/updated` frame to every subscribed connection via `encode` (encMu-serialized).

- [ ] **Step 1: Write failing test** — after a subscribe, `pushResourceUpdated("fuse://tools")` delivers a well-formed id-less `notifications/resources/updated` frame to the subscribed connection; an unsubscribed connection receives nothing.
- [ ] **Step 2: Run test, verify it fails.**
- [ ] **Step 3: Implement** per-connection subscription set + push helper.
- [ ] **Step 4: Run tests, verify pass.**
- [ ] **Step 5: Commit.**

## Task 6: Config-watch → registry rebuild → push (the real mutation, D5 item 7)

**Files:**
- Modify: `cmd/fuse/mcp_server.go`
- Test: `cmd/fuse/mcp_server_resources_test.go`

**Interfaces:**
- Consumes: the config path + `defaultToolRegistry()`; `Server.pushResourceUpdated`.
- Reference pattern: `internal/tui/mcp_provider.go applyConfigDiff` + fsnotify + 200ms debounce.
- Produces: a config change rebuilds the native registry; when the tool set changes, `fuse://tools` pushes an update to subscribed connections.

- [ ] **Step 1: Write failing test** — a config-watch rebuild that changes the tool set produces exactly one push for `fuse://tools`; an unchanged rebuild pushes nothing.
- [ ] **Step 2: Run test, verify it fails.**
- [ ] **Step 3: Implement** the watch → rebuild → diff → push, mirroring `applyConfigDiff`. Grep for all `defaultToolRegistry`/registry-build sites (all-sites rule) and confirm the watch attaches at the right one.
- [ ] **Step 4: Run tests, verify pass.**
- [ ] **Step 5: Commit.**

## Task 7: TUI stale/updated indicator

**Files:**
- Modify: `internal/tui/` (the MCP-facing view/model)
- Test: `internal/tui/…_test.go`

**Interfaces:**
- Consumes: `Manager.OnResource(ResourceObserver)`; the existing bridge/event plumbing (mirror how `OnProgress` reaches the TUI).
- Produces: a rendered stale/updated indicator for a subscribed resource.

- [ ] **Step 1: Write failing test** — a `ResourceUpdatedEvent` delivered to the model renders a stale/updated indicator in `View()`.
- [ ] **Step 2: Run test, verify it fails.**
- [ ] **Step 3: Implement** the observer subscription + indicator.
- [ ] **Step 4: Run tests, verify pass.**
- [ ] **Step 5: Commit.**

## Task 8: Dogfood end-to-end + TUI screenshots (the human's required evidence)

**Files:**
- Test: `internal/tui/mcp_resource_subscriptions_e2e_test.go` (or extend `mcp_tui_e2e_test.go`)
- Reference: `internal/tui/harness_test.go` `captureFrame`; `verify-tool-loop-at-gateway-seam`.

**Interfaces:**
- Consumes: `teatest.NewTestModel`, `teatest.WaitFor`, `captureFrame` (FinalModel().View() + TrueColor + freeze).

- [ ] **Step 1: Write the e2e test** — a real fuse client subscribes to a real `fuse mcp-server`'s `fuse://tools`; a live-reload on the server pushes the update; the client routes it; the TUI renders the stale/updated indicator. Use a hermetic real MCP server (re-exec helper per `verify-tool-loop-at-gateway-seam` note 3) rather than a fixture.
- [ ] **Step 2: Capture screenshots** — `captureFrame` at (a) subscribed/steady state and (b) post-push stale/updated state, writing `.ansi`/`.txt`/`.png` to `FUSE_SCREENSHOT_DIR`.
- [ ] **Step 3: Run the e2e test with `FUSE_SCREENSHOT_DIR` set, verify pass + screenshots produced.**
- [ ] **Step 4: Run the full suite (`go test ./... `, then `-race` on the mcp/tui packages) — verify green.**
- [ ] **Step 5: Commit.**

---

## Verification checklist (before PR)

- [ ] `go build ./...` and `go test ./...` green; `-race` green on `internal/mcp` and `internal/tui`.
- [ ] Client: list/read/subscribe/unsubscribe + ref-count + reconnect re-subscribe + stale-flag/no-auto-read all covered.
- [ ] Server: `fuse://tools` list/read, per-connection subscribe, id-less push on config-rebuild all covered.
- [ ] Dogfood e2e drives the real binary/seam; TUI screenshots captured to `FUSE_SCREENSHOT_DIR` as durable evidence for the results file + PR.
- [ ] All-sites grep re-run for any `cmd/fuse` tool-surface wiring touched.
