---
id: 53
slug: persistent-conversational-loop
title: Persistent conversational loop — interactive mode so one loop_id carries a multi-turn chat
status: implemented
priority: medium
type: feat
created: 2026-08-10
updated: 2026-08-11
depends_on: [48]
related: [46, 47, 49, 54]
discovered_from: [48]
adrs: []
spec: docs/superpowers/specs/2026-08-10-persistent-conversational-loop-design.md
plan:
results:
trivial: false
auto_groomable:
branch: feat/networked-runtime-binding
pr: 51
blocked_by:
reconciled: true
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-08-10-persistent-conversational-loop-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-08-10-persistent-conversational-loop-design.md) |
<!-- docket:artifacts:end -->

## Why

A fuse loop is a **single task run to completion**: the agent loop returns at its terminal
turn (the model produces text with no tool calls), the run goroutine exits, the durable
registry marks the loop finished, and its event store closes. That is exactly right for a
one-shot job — and it is the contract every binding relied on before now.

But the moment change 0048 exposed the loop over the network as a **chat** surface, the
gap showed: a client that answers "find me a Tulum rental" and then wants to ask "what
about Aspen?" has no primitive for it. `loop.send` to a finished loop returns
`ErrLoopFinished` (correctly — its injector no longer drains). The only workaround is a
**fresh `loop.start` per turn** with the whole prior conversation re-serialized into the
task prompt: N loop_ids, N event streams, history re-shipped by the client every turn, no
server-authoritative session. For the "attach to your running loop from your phone"
north-star (0048's Why), a conversation must be a **first-class, resumable, server-side
thing** — one `loop_id`, one event stream, history the server holds.

This was found the hard way: building the 0048 concierge demo, the follow-up turn failed
with `runtime: loop finished`, and the *next* problem after fixing it (a UI hang) traced
to the same root — there is no deterministic "this exchange is done, send the next
message" signal, because a one-shot loop never had to emit one. Both are symptoms of the
same missing primitive: a loop that **persists across turns**.

This is a small, self-contained runtime capability that 0049 (auth/identity) and 0050
(client SDK) both want underneath them — a session is the natural unit those layer onto —
so it is carved out as its own change rather than smuggled into 0048's transport PR.

## What changes

An opt-in **Interactive** loop mode over the existing Runtime seam — no new transport, no
new handle type. See the linked spec for the full design; at proposal altitude:

- **Park instead of finish.** In interactive mode, at the terminal turn boundary (no tool
  calls) the loop does not return: it **parks** awaiting the next human message, then loops
  back so the next turn's existing top-of-loop human-injector `Poll` (ADR-0022 self-pull)
  injects it as a user turn. `messages` (the full transcript) is carried forward unchanged,
  so history is **server-authoritative**. The loop ends only on context cancellation
  (client disconnect / shutdown) or a bus-less loop. Non-interactive runs are
  byte-identical to today (ADR-0016 run-to-completion preserved).
- **Wake without polling.** The per-node human-message bus (ADR-0022) gains a cap-1 notify
  channel: `loop.send` → `Enqueue` signals it, and the parked loop blocks on it rather than
  busy-polling. Orthogonal to the existing `Poll`/`Drain` path.
- **Uncapped turns for a conversation.** An interactive loop must run with unlimited
  `maxTurns` — each resumed exchange consumes real turns, so the finite headless backstop a
  binding bakes in would truncate the whole conversation at the cap. The runtime lifts the
  cap when it enables interactive mode, independent of any per-turn backstop a one-shot
  binding wants.
- **A deterministic completion event.** A new `loop.parked` event (`event.KindLoopParked` +
  `LoopParkedPayload{Turn, Content}`) is emitted just before the park, carrying the final
  answer. This is the reliable "exchange complete, send your next message" signal a
  conversational client keys on — reconstructing completion from the *shape* of the event
  stream desyncs once the loop persists (no run-end, no store-close between turns).
- **Wired through binding #3.** `runtime.LoopConfig.Interactive` flows from the loopserver
  `loop.start` param `"interactive": true`. Stdio clients (binding #2) omit it, so they stay
  single-task run-to-completion.
- **Proven by a real client.** An `examples/concierge-demo` web app drives the interactive
  loop over WebSocket end-to-end: multi-turn on one loop_id, live event tail, rental cards,
  and card-link grounding against real `tool.result` URLs.

## Out of scope

- **Auth / who-owns-the-session / tenant identity** — change 0049. Interactive mode carries
  `tenant_id` present-but-unenforced exactly as 0048 does; a session is a thing 0049's
  ownership model layers on top of, not something this change adjudicates.
- **A versioned external session envelope / client-SDK session ergonomics** — change 0050.
  This change adds the runtime primitive; the SDK wraps it.
- **Surviving client disconnect / resume-by-refresh — change 0054.** A parked loop blocks
  on the **per-connection context** (`ServeWS` → `handleStart(ctx)` → `StartLoop(ctx)` →
  `a.Run(ctx)`), so a client disconnect cancels it: the park returns, the registry flips the
  loop to finished, and the in-memory transcript is discarded. So this change makes a
  conversation survive **idle** (no message between turns), NOT **disconnect**. A user
  refreshing the page and getting their transcript + loop memory back — the "attach from
  your phone" north-star in the Why above — requires decoupling session lifetime from the
  connection, transcript durability/rehydration, and likely REST/HTTP resume endpoints.
  That is filed as **change 0054 (durable, resumable sessions)**.
- **Cross-instance session migration / durable park state.** A parked loop lives in its
  owning process's memory (the transcript is in-memory). Resuming a *finished/evicted*
  session from durable history on a cold instance is a larger question (registry liveness +
  transcript rehydration) — owned by change 0054.
- **Idle/session timeout, backpressure on an abandoned parked loop, max concurrent
  sessions** — operational policy on top of the primitive.
- **Streaming token deltas** (`model.delta`) for the chat surface — independent; the demo
  renders whole answers.

## Note on current state

The design in the linked spec is implemented on the `feat/networked-runtime-binding`
branch (0048's branch) as a single commit (`7ae2cb1`,
`feat(runtime): #53 persistent conversational loop — interactive mode + loop.parked`)
alongside the concierge demo, because it was built to make that demo prove multi-turn
end-to-end.

**Reconciled 2026-08-11:** that commit had been built and committed locally but never
pushed, so for a time it was absent from PR #51 despite this note asserting it rode along.
The commit has now been pushed onto `feat/networked-runtime-binding` (a clean fast-forward;
build + `vet` + affected-package tests green locally beforehand), so it rides 0048's PR
(#51) as intended. Status is `implemented`, PR `51`. This change did not go through the
docket build/review lifecycle as an independent change — the writeup + the reconciliation
are the deliverable here; it closes out to `done` when #51 merges.
