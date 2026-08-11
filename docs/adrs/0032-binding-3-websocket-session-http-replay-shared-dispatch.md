---
id: 32
slug: binding-3-websocket-session-http-replay-shared-dispatch
title: Binding #3 transport — WebSocket full-session + thin stateless HTTP replay over a shared dispatch core
status: Accepted
date: 2026-08-10
supersedes: []
reverses: []
relates_to: [28, 30, 31]
change: 48
---

## Context

The `runtime.Runtime` seam (change #45) is a policy-free interface with two bindings: the CLI (binding #1) and the headless stdio JSON-RPC `fuse loop-server` (binding #2, `internal/loopserver`). Binding #2 speaks the `loop.*` JSON-RPC protocol (`loop.start`/`loop.send`/`loop.observe` + server-push `loop.event`, with subscribe-before-replay + dedup-at-watermark + gap markers) but ONLY over `os.Stdin`/`os.Stdout` — unreachable over the network.

The hosting arc (attach to a running loop from another machine after a redeploy) needs the identical seam exposed over a network transport, building on #46 (multi-loop host) and #47 (durable/distributed event store, merged). This is the load-bearing "same seam, three bindings — no seam change" proof.

## Decision

Add binding #3 as a network transport that REUSES binding #2's exact `loop.*` JSON-RPC protocol and event framing — not a new protocol.

Extract a transport-agnostic dispatch core from `internal/loopserver.Server` (previously coupled to `*json.Encoder`/`*json.Decoder` over stdio): a `conn` abstraction (`transport.go`) exposing `readRequest(ctx) (req, error)` — `io.EOF` ends the loop — and `encode(v any) error`, which OWNS the one-mutex-per-connection write serialization that keeps a mid-observe id-less `loop.event` notification from interleaving with another write. Stdio becomes one transport (`stdio.go`, `newStdioTransport`); the network is a second (`net.go`). `NewServer` keeps its signature; binding #2 stays behaviorally identical and `internal/mcp` is byte-untouched.

The subscribe-before-replay + dedup-at-watermark (`ev.Seq <= last`) + gap-marker (`ev.Seq > prev+1 && prev != 0`) discipline in `serveObserve` is inherited VERBATIM by the network transport — never reinvented.

Transport shape (D3): one WebSocket connection carries the full `loop.*` session (`start`/`send`/`observe` + the server-push `loop.event` tail) — the WS conn IS the bidirectional stream the stdio core already assumes. HTTP is a thin STATELESS replay endpoint only — `GET /loops/{id}/events?from=<seq>` maps directly to `rt.Attach(loop_id, from)` for catch-up-only late joiners; no `loop.start`/`loop.send` on HTTP (splitting `loop.send` across two transports would reintroduce the cross-transport ordering problem the single stream avoids).

Reconnect is client-driven and the server holds NO per-connection resume state (D4): the client tracks its last `event.Seq` and re-observes `from=<lastSeq>`; the inherited dedup path guarantees no loss / no dup. Wire format is inherited (D5): `event.Event`'s existing JSON encoding, `loop_id` (`tree.RootID()`) as the handle — no new envelope. WS library (D6): `github.com/coder/websocket` (context-first, minimal deps) over `gorilla/websocket`.

The subcommand `fuse loop-serve-net` wires through the SAME composition root (`buildLoopServerRuntimeDeps` + `runtime.New`) as binding #2; auto-approve is the binding's documented policy (no human on a TTY). `tenant_id` flows through present-but-unenforced (identity is #0049).

## Consequences

Enables remote loop hosting over the network reusing a proven protocol — proves the `Runtime` seam is portable across a third transport with no interface change (validates ADR-0030's policy-free seam). Costs a new dependency (`coder/websocket`) and a small refactor of `internal/loopserver` into transport/stdio/net files.

Given up for now (deferred): auth/identity (#0049), REST-native start/send + versioned SDK envelope (#0050), observability emission (#0051), TLS/deployment topology. The HTTP replay endpoint returns raw error strings and uses `coder/websocket`'s default same-origin `CheckOrigin` — acceptable at this pre-auth stage, to be hardened alongside #0049.

Relates to ADR-0028 (binding #2 chose JSON-RPC-not-MCP — same "reuse proven framing, own the dispatch" spirit), ADR-0030 (the de-globalized, policy-free multi-loop seam this transport is proven portable against), and ADR-0031 (the durable/distributed event store this hosting arc attaches over).
