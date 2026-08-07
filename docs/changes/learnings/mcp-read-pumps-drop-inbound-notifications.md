---
name: mcp-read-pumps-drop-inbound-notifications
slug: mcp-read-pumps-drop-inbound-notifications
title: fuse's MCP read pumps silently drop inbound notifications — id-less frames are skipped, not routed
hook: "fuse's MCP client read pumps (StdioClient.readPump, httpClient.readSSEPump) match responses by id and silently drop any id-less frame — so inbound server notifications (`$/progress`, `notifications/resources/updated`) are discarded today; adding a notification *sender* does not make fuse *receive* them — a notification route must be added to the pump itself, not just a handler elsewhere"
promotion_state: candidate
changes: [19]
created: 2026-08-07
updated: 2026-08-07
topics: [mcp, notifications, json-rpc, go, streaming, subscriptions]
---

Change 0019 added the MCP init handshake and an outbound `notify()` path (for the
id-less `notifications/initialized` message). Outbound notifications are easy: a
distinct `jsonrpcNotification` frame with no `id` field (the existing
`jsonrpcRequest.ID` is not `omitempty`, so it cannot express one) posted/encoded
without registering a pending channel.

The **inbound** direction is the trap. Both client read pumps are built purely as
response demultiplexers keyed on `id`:

- `StdioClient.readPump` decodes a `jsonrpcResponse`, looks up `c.pending[resp.ID]`,
  and if there is no match it drops the frame on the floor.
- `httpClient.readSSEPump` is explicit: `if resp.ID == "" { continue // notification }`.

So a server-initiated notification — which by definition has **no `id`** — is
silently discarded today. Nothing is wired to observe it. This is invisible until
a feature actually needs to *receive* one:

- **#0020 (`$/progress` streaming)** — progress notifications arrive id-less mid
  tool-call; the pump drops them, so no amount of caller-side handler wiring will
  see progress until the pump grows a notification route.
- **#0021 (resource subscriptions)** — `notifications/resources/updated` is the
  entire point of the feature and is id-less; same trap.

**Rule that must fire unprompted:** when adding *inbound* MCP notification support,
change the read pump to branch on "no id" and dispatch to a notification router —
do not assume a handler registered elsewhere will be reached, and do not model it
as a `call()`/response (there is no id to correlate). Confirm the fix against the
real `cmd/fuse` seam (`fuse mcps list --live` drives the real manager), since the
in-package harness can fake the pump.
