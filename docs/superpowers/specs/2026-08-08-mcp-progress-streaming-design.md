<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0020 — MCP `$/progress` notifications and streaming tool results](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0020-mcp-progress-streaming.md)**
<!-- docket:backlink:end -->

# MCP `$/progress` notifications and streaming tool results

**Change:** [#0020](../../changes/active/0020-mcp-progress-streaming.md) · **Status:** design · **Date:** 2026-08-08

## Problem

Long-running MCP tool calls (builds, code execution, multi-step retrievals) are **opaque**:
`MCPTool.Execute` (`internal/mcp/tool.go:39`) issues a blocking `client.call(ctx, "tools/call",
…)` and the caller waits until the tool completes with zero intermediate feedback. The MCP
**2025-03-26** spec formalizes two server→client notifications for this:

- **`$/progress`** — `{ progressToken, progress (number), total? }` emitted mid-call to report
  determinate/indeterminate progress.
- **`$/stream`** — same envelope plus a `delta` field carrying an incremental content chunk, so
  a tool can stream partial results as they are produced.

fuse should **receive** these (client side) and render live progress in the agent tree, and its
own MCP **server** should **emit** them for its tools. The result: progress bars per running
tool node and a rolling partial-content line for streaming tools, while the agent loop still
sees the same complete result it does today.

### The load-bearing prerequisite: the read pumps drop id-less frames

**fuse's MCP client read pumps discard every server-initiated notification today.** Both pumps
are pure response demultiplexers keyed on `id`:

- `StdioClient.readPump` (`internal/mcp/client.go:151`) decodes a `jsonrpcResponse`, looks up
  `c.pending[resp.ID]`, and **drops the frame** when there is no match.
- `httpClient.readSSEPump` (`internal/mcp/http_client.go:215`) is explicit:
  `if resp.ID == "" { continue // notification }`.

A `$/progress` / `$/stream` notification has **no `id` by definition**, so it is silently
dropped. **No amount of caller-side handler wiring receives progress until the pump itself grows
a notification route** (learning `mcp-read-pumps-drop-inbound-notifications`, which names this
change). Adding the pump route is therefore the foundational task, and it must be verified
against the **real `cmd/fuse` seam** (`fuse mcps list --live` drives the real manager) because
the in-package harness fakes the pump.

## Relationship to #0019 and #0021

- **#0019 (capability negotiation) is `done` (PR #23 merged 2026-08-07)** and landed the exact
  prerequisites this builds on: the id-less **`notify`** send path on `mcpConn`,
  **`ServerCapabilities` + `Supports(key)`** (fail-open, ADR-0010), and the `2025-03-26`
  handshake. *Local-sync note:* if a working checkout's `main` predates the #23 merge, the
  implementer's reconcile pass pulls fresh `main` — the seams below are #0019's merged design.
- **#0021 (resource subscriptions)** hits the **identical** id-less-notification trap
  (`notifications/resources/updated`). The pump **notification router** built here is designed
  to be **feature-generic** so #0021 registers its handler on the same seam rather than
  re-solving it.

## Design decisions (settled)

The interactive brainstorm settled four points. `superpowers:brainstorming` was unavailable in
the grooming session, so the design was reached inline with the human (docket Skill-layer
missing-skill fallback) and is recorded here as final.

- **D1 — Scope: both notifications, both sides.** `$/progress` **and** `$/stream`, on **both**
  the client (receive/route) and fuse's MCP server (emit) sides. One PR covers the whole
  2025-03-26 streaming surface.
- **D2 — Capability-gated, fail-open.** The client injects a `progressToken` and expects
  streaming **only when** `Supports("streaming")` is true; a non-advertising server gets today's
  blocking behavior. Never reject over capability mismatch — degrade silently (ADR-0010).
- **D3 — Hybrid buffering.** `$/stream` chunks feed a **ring buffer for live TUI display**, but
  the agent **loop receives the concatenated complete result** as the tool-result value — the
  loop's contract is unchanged, and Tier-1 spill/prune still apply to the final value.
- **D4 — Feature-generic notification router.** The pump route dispatches by notification
  `method` to registered handlers, so `$/progress`, `$/stream`, and (later) #0021's
  `notifications/resources/updated` share one seam.

## What we build

### 1. Inbound notification route in both read pumps (foundational)

Add a notification branch to **both** pumps, before the id-keyed response lookup:

- `StdioClient.readPump`: after decoding a frame, if it has **no `id`** and carries a `method`,
  dispatch it to the notification router instead of looking up `c.pending`.
- `httpClient.readSSEPump`: replace the `if resp.ID == "" { continue }` drop with the same
  dispatch.

Decode into a permissive `jsonrpcNotification { Method string; Params json.RawMessage }` (the
mirror of #0019's outbound notification frame). Malformed / unknown-method notifications are
logged and dropped — never fatal to the pump (fail-open).

### 2. Notification router (`internal/mcp/`, new — feature-generic, D4)

A small dispatcher owned by the manager, keyed by notification `method`:

```go
type NotificationHandler func(server string, params json.RawMessage)

// registered by feature: "$/progress", "$/stream", and later
// "notifications/resources/updated" (#0021)
func (m *Manager) OnNotification(method string, h NotificationHandler)
```

The pump calls into the router with the originating server name. Unknown methods are a no-op
(logged once). This is the seam #0021 reuses.

### 3. Progress-token minting and injection (client, D2)

When `Supports("streaming")` is true, `MCPTool.Execute` (`internal/mcp/tool.go:39`) mints a
unique `progressToken` per call and injects it into the `tools/call` params `_meta`:

```jsonc
{ "name": t.toolName, "arguments": params, "_meta": { "progressToken": "<token>" } }
```

The manager records `token → (AgentNode, ring buffer)` for the duration of the call and clears
it on completion. When `Supports("streaming")` is false, no token is injected and the call is
blocking exactly as today.

### 4. `$/progress` handler → agent-tree progress events

Registered on the router. On each `$/progress`, match `progressToken` to the tracked call and
emit a new **`ProgressEvent`** on the `AgentNode` (`progress`, optional `total`). The agent-tree
TUI renders a bar (`[████░░░░] 50%`) on the running tool's node; indeterminate progress (no
`total`) renders a spinner/percentage. Unmatched tokens are dropped (the call may have already
completed).

### 5. `$/stream` handler → hybrid buffering (D3)

On each `$/stream`, append `delta` to the call's **ordered ring buffer** and update a rolling
partial-content line in the TUI. Chunks are ordered by arrival on the single pump goroutine (no
reordering needed for stdio/SSE, which are ordered streams). On tool completion, the manager
delivers the **concatenated buffer** as the `tools/call` result value to the agent loop — so the
loop sees a complete result and spill/prune apply to it normally. The ring buffer is bounded;
overflow keeps head+tail with a truncation marker (consistent with `internal/tools/spill.go`).

### 6. fuse MCP server emits `$/progress` (server side, D1)

`Server.handleCall` (`internal/mcp/server.go:107`) gains a way for a tool implementation to emit
progress: a progress callback threaded into tool execution that, when the incoming call carried
a `_meta.progressToken`, encodes and writes a `$/progress` notification frame back over the same
transport (reusing the server's frame encoder at `server.go:67`). Capability-gated the same way
(the server already advertises its capabilities post-#0019; it emits progress only when the
client's call supplied a token). Tools with no progress to report simply never call the callback.

### 7. Config / capability surface

No new config knob for v1 — behavior is driven by the negotiated `Supports("streaming")`
capability from #0019. (A future opt-out could be added if a server misbehaves; not needed now.)

## Out of scope

- **`$/stream` for fuse's own non-MCP tool results** (`web_search`, `bash`) — only MCP-tool
  results stream (stub out-of-scope, retained).
- **Streaming partial child-agent prose into the parent** — an agent-runtime concern, not MCP.
- **Progressive partial results into the agent loop** — the loop receives the complete
  concatenated result (D3); progressive delivery into the loop is explicitly deferred (larger
  blast radius on the tool-result contract + pruning/spill).
- **Reordering out-of-order chunks** — stdio/SSE are ordered; no sequence-number reassembly.
- **Persisting progress/stream history across reconnects.**

## Tests

- **Pump route (the foundational fix)**: feed each pump an id-less `$/progress` frame and assert
  it reaches the router (not dropped); assert an id-keyed response still resolves its pending
  channel unchanged. Cover **both** `readPump` and `readSSEPump`.
- **Router dispatch**: registered method receives its params with the right server name; unknown
  method is a logged no-op; malformed params never panic the pump.
- **Token gating (D2)**: `Supports("streaming")` true ⇒ `tools/call` carries `_meta.progressToken`;
  false ⇒ no token, blocking behavior identical to today (golden comparison).
- **`$/progress` → tree**: a matched token emits a `ProgressEvent` on the right `AgentNode`;
  determinate (`total` set) vs indeterminate render paths; unmatched token dropped silently.
- **`$/stream` hybrid (D3)**: ordered chunks assemble into the ring buffer; the TUI shows the
  rolling partial line; **the agent loop receives the concatenated complete result**; ring-buffer
  overflow keeps head+tail with a truncation marker.
- **Server emit (D1)**: a fuse-server tool that calls its progress callback writes a well-formed
  id-less `$/progress` frame back over the transport only when the call supplied a token.
- **Real-binary seam** (learning `mcp-read-pumps-drop-inbound-notifications`): drive
  `fuse mcps list --live` (or the real manager path) against a scripted MCP server double that
  emits `$/progress` + `$/stream` mid-call, asserting fuse receives and renders them — the
  in-package harness fakes the pump and would mask a missing route.
- **TUI render** (learning `teatest-final-frame-via-finalmodel-view`): progress bar and rolling
  partial line render via the final model's `View()` with `termenv.TrueColor` forced; untrusted
  chunk bytes are sanitized/wrapped (`sanitize-untrusted-bytes-fixed-width-tui`).

## Risks & mitigations

- **The pump silently drops notifications** (the dominant risk) — mitigated by making the pump
  route task #1 and verifying it against the real `cmd/fuse` seam, not the in-package fake.
- **Token mismatch / late notifications** — unmatched tokens are dropped silently; the call's
  tracking entry is cleared on completion so a straggler cannot mutate a finished node.
- **Unbounded stream buffer** — bounded ring buffer with head+tail truncation, mirroring spill.
- **Chunk corruption in a fixed-width TUI** — sanitize/wrap untrusted chunk bytes before display.
- **Capability mismatch** — fail-open per ADR-0010: no token, blocking call, no error.

## Follow-ups (not this change)

- #0021 (resource subscriptions) registers `notifications/resources/updated` on the router built
  here.
- Optional progressive-partial delivery into the agent loop, if a use case demands it.
- `$/stream` for fuse's own non-MCP tools, if valuable.
