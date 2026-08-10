---
id: 28
slug: loopserver-new-jsonrpc-server-not-mcp-extension
title: Binding #2 is a new internal/loopserver JSON-RPC server, not an extension of internal/mcp
status: Accepted
date: 2026-08-10
supersedes: []
reverses: []
relates_to: [27]
change: 45
---

## Context

Change #45 adds a second binding over the Runtime seam: a headless `fuse
loop-server` stdio subcommand exposing `loop.start` / `loop.send` /
`loop.observe` as JSON-RPC methods, with `loop.observe` pushing live
`loop.event` notifications. The spec required the existing tool-call MCP server
(`cmd/fuse/mcp_server.go`) and `internal/mcp` to stay BYTE-UNTOUCHED (an
out-of-scope invariant). The natural question was whether to host the loop.*
methods on the existing `internal/mcp.Server`.

Investigation found `internal/mcp.Server` is a closed struct with a FIXED
dispatch switch over the MCP method set (initialize / tools/list / tools/call /
resources/*) and no hook for registering arbitrary custom methods; its only
server→client push is `PushResourceUpdated`, gated on a `fuse://tools`
subscription. So loop.* could not be added without modifying `internal/mcp`,
violating the byte-untouched invariant, and loop.observe's live-tail
notifications don't map onto the resources/updated mechanism cleanly.

## Decision

Binding #2 is a NEW, self-contained package `internal/loopserver` — a minimal
stdio JSON-RPC 2.0 server that is a pure adapter over `runtime.Runtime`. It
reuses the PROVEN wire-framing discipline of `internal/mcp` (the same
request/response struct shapes and a single encoder-mutex serializing every
write, including notifications) but is an independent server with its OWN
dispatch switch over the `loop.*` methods and its own `loop.event` notification
frame. `internal/mcp` and `cmd/fuse/mcp_server.go` are left byte-identical
(guarded by `git diff --exit-code` in a test).

`internal/loopserver` imports only `internal/runtime` + `internal/event` — no
renderer/TUI/MCP-tool-registry type — so binding #2 stays headless and
policy-free (its "no approval gate / auto-approve" stance is the binding's
documented choice, not a property of the Runtime seam). Decode-error handling is
fail-fast (a streaming json.Decoder cannot resync mid-stream) emitting a -32700
parse-error frame then tearing down the connection.

## Consequences

- (+) The tool-call MCP surface stays byte-untouched — no regression risk to the
  existing `mcp-server` subcommand or `internal/mcp`.
- (+) Binding #2 is a clean, isolated proof that the Runtime seam is reusable by
  a second, policy-different consumer (headless, no gate/renderer/TUI) — the core
  "two bindings, one seam" thesis.
- (+) The loop-control protocol (`loop.start`/`loop.send`/`loop.observe` +
  `loop.event` notifications with gap markers and replay/live dedup) can evolve
  independently of the MCP tool-call protocol.
- (−) Two stdio JSON-RPC servers now coexist (`internal/mcp` for tools,
  `internal/loopserver` for loop control) with some duplicated framing/encoder-
  mutex boilerplate; a future refactor could extract a shared JSON-RPC transport
  core if the duplication grows.
- (−) `internal/loopserver` is not a general MCP server — a client must speak the
  fuse loop-control method set directly; it is not discoverable via MCP
  `initialize`/`tools/list`.
