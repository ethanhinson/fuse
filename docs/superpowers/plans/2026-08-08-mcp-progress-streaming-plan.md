<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0020 — MCP `$/progress` notifications and streaming tool results](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0020-mcp-progress-streaming.md)**
<!-- docket:backlink:end -->

# Implementation plan — MCP `$/progress` notifications and streaming tool results (change 0020)

> Plan authored inline (docket plan auto-fallback: `superpowers:writing-plans` was
> unavailable on this machine; the plan role degraded to `auto` per the docket
> Skill-layer missing-skill rule). Spec:
> `docs/superpowers/specs/2026-08-08-mcp-progress-streaming-design.md` (on the `docket` branch).

## Context the executor must hold

Every seam below is verified against `origin/main` (tip `106b792`) at reconcile time:

- **Read pumps drop id-less frames today** (the load-bearing bug, learning
  `mcp-read-pumps-drop-inbound-notifications`, which *names* this change):
  - `StdioClient.readPump` (`internal/mcp/client.go:160`) decodes into `jsonrpcResponse`
    (which has **no `Method` field**), looks up `c.pending[resp.ID]`, drops on no match.
  - `httpClient.readSSEPump` (`internal/mcp/http_client.go:273-274`) is explicit:
    `if resp.ID == "" { continue // notification }`.
  A `$/progress` / `$/stream` frame is id-less by definition, so it is discarded. **The pump
  route is task 1** and must be verified against the real `cmd/fuse` seam, not the in-package fake.
- **#0019 prerequisites are present and merged** (`done`, archived): `jsonrpcNotification { Method; Params }`
  frame (`client.go:41`); `mcpConn.notify(ctx, method, params)` (`conn.go:14`); `ServerCapabilities.Supports(key)`
  (`capabilities.go:29`); per-server `caps` on the manager (`manager.go:56`).
- **`tools.Tool.Execute(ctx, args) tools.Result` carries no `AgentNode` and no progress channel**;
  `MCPTool` holds only an `mcpConn` (`tool.go:20`). So progress→node binding is **manager-mediated**,
  not threaded through `Execute` (reconcile-log nuance). The manager owns the `token → call-tracking`
  map and exposes an **observer/event seam** the TUI subscribes to (mirroring
  `internal/tui/mcp_provider.go` reaching `mgr`). We do NOT change the `tools.Tool` interface.
- **The fuse server advertises only `{"tools":{}}`** (`server.go` `initialize`) and its `enc` is a
  **single shared `json.Encoder`** — emitting an id-less `$/progress` mid-`handleCall` must be
  mutex-serialized against the response `Encode`, and the server must additionally advertise a
  `streaming` capability so a client's `Supports("streaming")` gate can pass.

## Verification disciplines (from learnings)

- Verify the client receive path with the **real `MCPTool.Execute → client.call → server`** round
  trip using a **hermetic re-exec MCP server double** (re-exec the test binary with
  `-test.run=<helper>` + an env flag, run a ~30-line JSON-RPC stdio loop, `os.Exit(0)` before the
  test framework prints its summary) — learning `verify-tool-loop-at-gateway-seam` §(3). The
  in-package harness fakes the pump and would mask a missing route.
- **Do NOT drive the shipped TUI via a PTY** (dead end); teatest in-process is the working path.
- TUI screenshot (if added): render `FinalModel().View()` (not `FinalOutput`) and force
  `termenv.TrueColor` — learning `teatest-final-frame-via-finalmodel-view`.
- **Sanitize every untrusted `$/stream` chunk** before any fixed-width render (strip ESC/C0/C1/CR,
  expand tabs, hard-wrap) — learning `sanitize-untrusted-bytes-fixed-width-tui`.

## Tasks (TDD — each task: failing test first, then implement, then `go test ./internal/mcp/...`)

### Task 1 — Inbound notification route in `StdioClient.readPump` (foundational)
- **Test first** (`internal/mcp/client_test.go`): feed the pump an id-less
  `{"jsonrpc":"2.0","method":"$/progress","params":{...}}` frame and assert it reaches a registered
  router callback (record server name + raw params); assert an id-keyed response still resolves its
  pending channel unchanged (regression); assert a malformed/unknown-method notification never
  panics the pump and is logged-and-dropped.
- **Implement**: introduce a permissive inbound frame that captures **both** `id` and `method`
  (e.g. decode into `struct{ ID string; Method string; Params json.RawMessage; Result …; Error … }`
  or decode into `json.RawMessage` then sniff). Branch: if `Method != "" && ID == ""` → dispatch to
  the router (see Task 3) with the client's server name; else the existing response path. Keep the
  decoder single-pass (`readPump` uses a streaming `json.Decoder`).

### Task 2 — Inbound notification route in `httpClient.readSSEPump`
- **Test first** (`internal/mcp/http_client_test.go` / existing streamable test seam): an SSE `data:`
  frame carrying an id-less `$/progress` reaches the router; an id-keyed response still fans to its
  pending channel; malformed JSON still skipped (unchanged).
- **Implement**: replace `if resp.ID == "" { continue }` (`http_client.go:273-274`) — decode the
  joined `data` payload into the same combined frame; route id-less `method` frames, else existing path.

### Task 3 — Feature-generic notification router on the Manager (D4)
- **Test first** (`internal/mcp/manager_test.go` or new `notification_router_test.go`): register a
  handler for `$/progress`; dispatch delivers `(server, params)`; a second registration for a
  different method is isolated; an unregistered method is a logged no-op; concurrent dispatch +
  register is race-clean (`-race`).
- **Implement**: `type NotificationHandler func(server string, params json.RawMessage)` and
  `func (m *Manager) OnNotification(method string, h NotificationHandler)` plus an internal
  `dispatchNotification(server, method, params)` the pumps call. Mutex-guarded map (mirror the
  `mutex-test-double-concurrent-provider` discipline). Wire each managed client's pump to the
  manager's dispatch at construction (`startAndDiscover`/`handshakeAndDiscover`).

### Task 4 — Progress-token minting + injection, capability-gated (D2)
- **Test first**: with a fake conn advertising `streaming`, `MCPTool.Execute` sends `tools/call`
  params carrying `_meta.progressToken`; without `streaming`, params carry **no** `_meta` (golden
  comparison — blocking behavior byte-identical to today).
- **Implement**: give `MCPTool` access to its server's `Supports("streaming")` (thread the caps or a
  `supportsStreaming bool` in from the manager at tool construction — do not change `tools.Tool`).
  When true, mint a unique token (`counter`/uuid), register `token → callTracking{server, ring buffer,
  observer fanout}` on the manager, inject `"_meta":{"progressToken":token}` into the `tools/call`
  params, and clear the tracking entry on return (defer). When false, unchanged path.

### Task 5 — `$/progress` handler → progress observer event (manager-mediated node binding)
- **Test first**: a `$/progress` with a matched token fans a `ProgressEvent{Server, Tool, Progress,
  Total(optional)}` to the manager's registered observer; an unmatched/late token is dropped silently
  (no panic, no mutation of a cleared entry); determinate (`total` set) vs indeterminate both deliver.
- **Implement**: register the `$/progress` handler on the router (Task 3). Define `ProgressEvent` and
  a manager observer seam (`func (m *Manager) OnProgress(func(ProgressEvent))` or a channel) the TUI
  layer subscribes to. **No node handle flows through `Execute`** — the TUI correlates the event to
  the running tool node by (server, tool/token) the way `mcp_provider.go` already reaches the manager.

### Task 6 — `$/stream` handler → hybrid buffering (D3)
- **Test first**: ordered `delta` chunks assemble into the call's bounded ring buffer; on completion
  the **agent loop receives the concatenated complete result** (loop contract unchanged); ring-buffer
  overflow keeps head+tail with a truncation marker (consistent with `internal/tools/spill.go`);
  a rolling partial line is exposed for the TUI. Chunk bytes are **sanitized** before the TUI line.
- **Implement**: register the `$/stream` handler; append `delta` to the tracked call's ordered ring
  buffer (single pump goroutine ⇒ no reordering); expose a rolling-partial accessor; on `tools/call`
  return, if any stream chunks arrived, deliver the concatenated buffer as the result value.

### Task 7 — fuse MCP server emits `$/progress` (server side, D1)
- **Test first** (`internal/mcp/server_test.go`): a tool that calls its injected progress callback
  causes the server to write a well-formed id-less `$/progress` frame back over the transport **only
  when** the incoming `tools/call` carried `_meta.progressToken`; no token ⇒ no frame; the frame
  write is serialized against the response `Encode` (no interleaving corruption); the server now
  advertises a `streaming` capability at `initialize`.
- **Implement**: parse `_meta.progressToken` in `handleCall`; thread a `func(progress float64, total
  *float64)` progress callback into tool execution that encodes a `jsonrpcNotification{Method:
  "$/progress", Params:{progressToken, progress, total?}}` via the shared `enc` under a new
  `s.encMu sync.Mutex` (guard **both** the response `Encode` in `Serve` and the notification write).
  Add `"streaming": map[string]any{}` (or `true`) to the `initialize` capabilities map. Tools with
  no progress simply never invoke the callback.

### Task 8 — Real-binary seam verification (the load-bearing check)
- Add a test (`internal/mcp/…_live_test.go` or `internal/tui/mcp_tui_e2e_test.go` sibling) that drives
  the **real manager** against the **hermetic re-exec MCP server double** which emits `$/progress`
  (+ optionally `$/stream`) mid-call, asserting fuse **receives and routes** them (observer sees the
  `ProgressEvent`; stream buffer concatenates) — proving the pump route works end-to-end, not just in
  the faked-pump unit. This is the mitigation the spec and the learning both demand.

### Task 9 — Full-suite gate
- `go build ./...`, `go vet ./...`, `go test ./...` (and `go test -race ./internal/mcp/...` for the
  concurrency-touching pump/router/server-encoder paths). All green before the PR opens.

## Out of scope (spec §Out of scope — do not implement)
- `$/stream` for fuse's own non-MCP tools (`web_search`, `bash`).
- Streaming partial child-agent prose into the parent.
- Progressive partial results **into the agent loop** (loop gets the concatenated complete result).
- Reordering out-of-order chunks; persisting progress/stream across reconnects.

## Notable deviations from the spec (fold-ins, not design changes)
- The spec's "emit a `ProgressEvent` on the `AgentNode`" / "manager records `token → (AgentNode, ring
  buffer)`" is realized as a **manager observer seam** (Task 5), because `Execute` has no node handle
  — the node correlation lives in the TUI layer that already reaches the manager. Same behavior, seam
  relocated to where the wiring actually exists.
- The server must additionally **advertise `streaming`** (Task 7) so the client's `Supports("streaming")`
  gate (D2) can ever pass against fuse's own server — implied by D1+D2 but not spelled in the spec.
