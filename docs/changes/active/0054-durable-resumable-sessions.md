---
id: 54
slug: durable-resumable-sessions
title: Durable, resumable sessions — a conversation survives disconnect; refresh restores transcript + memory
status: proposed
priority: medium
type: feat
created: 2026-08-11
updated: 2026-08-11
depends_on: [53]
related: [47, 48, 49, 50]
discovered_from: [53]
adrs: []
spec:
plan:
results:
trivial: false
auto_groomable:
branch:
pr:
blocked_by:
reconciled: false
---

## Why

Change 0053 made a conversation persist **across turns** on one `loop_id` — but only
while its owning goroutine and its **in-memory transcript** stay alive, and only while the
originating connection stays up. The parked loop blocks on the **per-WebSocket-connection
context** (`ServeWS` → `handleStart(ctx)` → `StartLoop(ctx)` → `a.Run(ctx)`), so a client
disconnect cancels that ctx: the park falls through to the terminal return, the registry
flips the loop to finished (a later `loop.send` ⇒ `ErrLoopFinished`), and the in-memory
`messages` transcript is discarded. Cleanup is correct and leak-free — but the session is
**pinned to the liveness of one connection**.

That directly contradicts 0053's own north-star framing ("attach to your running loop from
your phone"; a conversation as a "first-class, resumable, server-side thing"). As shipped,
you cannot **refresh the page and get your conversation back**. Close the tab, lose the
session. 0053 explicitly scoped this out ("Cross-instance session migration / durable park
state ... deferred until 0049/0050 need it") — this change is where that deferred work is
owned.

The user-visible requirement: a user should be able to **refresh the screen (or reconnect
from another device) and see their transcript, with the loop's memory intact**, then keep
chatting on the same session.

## What changes (proposal altitude — design in brainstorm)

The gap has two distinct halves, because two different things are lost on disconnect:

1. **Event history** — already durable. The runtime seam exposes `Attach(loopID, from)`
   (EventStore.Replay over the 0047 durable store), so the *event stream* of a session
   already survives and can be replayed to a reconnecting client. This half is largely a
   wiring/UX question, not new persistence.
2. **The agent's model-facing transcript (`messages`) and park state** — NOT durable. This
   lives only in the running goroutine's memory. Resuming a finished/evicted session on a
   warm or cold instance requires **rehydrating the transcript** (either persisted
   directly, or reconstructed from the durable event stream) and **re-parking** a loop for
   that `loop_id` so `loop.send` resumes instead of returning `ErrLoopFinished`.

Likely scope to settle in design:

- **Decouple session lifetime from connection lifetime.** A parked interactive loop should
  survive a client disconnect (bounded by an idle/session timeout — the operational policy
  0053 also scoped out), rather than being cancelled by the per-connection ctx. This means
  a session-scoped context distinct from the WS-connection context.
- **Transcript durability + rehydration.** Persist enough to reconstruct the model-facing
  `messages` for a `loop_id` (persist the transcript, or derive it from the durable event
  stream), and a path to resume a loop from it.
- **REST/HTTP resume surface.** The current HTTP binding is **read-only replay**
  (`GET /loops/{id}/events`); WS owns all mutation. Refresh-to-restore likely wants
  first-class HTTP endpoints — e.g. fetch a session's transcript/state, list a user's
  sessions, and resume/re-attach — so a plain page load (no live WS yet) can render history
  before upgrading to the live tail. Decide the REST surface vs. leaning entirely on the WS
  reconnect + HTTP replay already present.
- **Ownership / identity.** "A user's sessions" and "resume my session from another device"
  presume the 0049 tenant/ownership model. This change layers on 0049 rather than
  re-litigating identity.

## Out of scope

- **Who owns a session / tenant enforcement** — change 0049. This change consumes 0049's
  ownership model; it does not define it.
- **Client-SDK session ergonomics** (a versioned external session envelope, resume helpers)
  — change 0050 wraps whatever primitive/REST surface this change lands.
- **Streaming token deltas** for the restored view — independent (0053 out-of-scope too).

## Note

Filed 2026-08-11 while reconciling 0053 (PR #51). 0053's park mechanism is correct and
non-leaking for the connected case; this change lifts the "survives disconnect / resume by
refresh" requirement that 0053 deferred. Needs a brainstorm to settle the persistence
model (persist transcript vs. rebuild from events) and the REST surface before it is
build-ready.
