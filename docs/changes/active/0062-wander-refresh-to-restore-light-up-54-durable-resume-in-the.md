---
id: 62
slug: wander-refresh-to-restore-light-up-54-durable-resume-in-the
title: 'Wander refresh-to-restore — light up #54 durable resume in the browser demo'
status: proposed
priority: medium
type: feat
created: 2026-08-15
updated: 2026-08-15
depends_on: [54]
related: [50, 56, 60]
discovered_from: [54]
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

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
<!-- docket:artifacts:end -->

## Why

Change #54 landed durable, resumable sessions on the server: an interactive loop
survives client disconnect, its transcript is rebuilt from the durable event stream,
and a reconnecting client re-observes from seq 0 (full replay) then keeps chatting via
Send (transparent Resume). This was verified end-to-end against the real `loop-serve-net`
server across a real disconnect + multi-turn conversation.

The **Wander example app deliberately stops exactly where #54 begins.** Its own docs say
so: `examples/wander/README.md` and `app.js` note "Wander is **stateless across page
loads (#54 boundary)**: a refresh starts a fresh loop … it demonstrates a *live,
reconnecting* session **without needing durable/resumable sessions (change #54)**." Wander
uses interactive loops + mid-stream network reconnect (re-observe from last seq, #56), but
a **page refresh mints a brand-new loop** — so the user loses their conversation on reload,
the precise gap #54 closed on the server.

Now that the server-side capability exists and is proven, Wander can be upgraded from
"refresh = fresh session" to "**refresh = restore my conversation**": persist the `loopId`
across page loads (e.g. `localStorage`) and, on load, re-`observe(loopId, { fromSeq: 0 })`
to replay the transcript before resuming, instead of calling `startLoop`. This turns Wander
into the human-visible demonstration of #54 — the "close the tab, reopen, keep chatting"
story — and dogfoods the resume path through the real browser SDK.

## What changes (proposal altitude — design in brainstorm)

- **Persist `loopId` across page loads** (localStorage or similar) so a reload knows which
  session to restore; mint a fresh loop only when none is stored or the stored one is gone.
- **On load, restore before resume:** `observe(loopId, { fromSeq: 0 })` to replay the durable
  transcript into the UI, render it, then continue live — rather than `startLoop`.
- **Handle the terminal cases** the SDK already classifies (#56, ADR-0037): a stored `loopId`
  that is `not_found` (evicted past its bound / server wiped) falls back to a fresh session
  cleanly, without a hot-loop or a broken UI.
- **Update the README's "#54 boundary" note** once Wander crosses it, and extend the
  headless-browser CI lane to cover a *reload* (new page context, same stored loopId →
  transcript restored), not just a mid-stream network drop.
- **Decide the idle-TTL UX:** #54's default session idle-TTL is 30 min; a reload after that
  window is a legitimately-expired session. Decide how Wander surfaces "your session expired,
  starting fresh" vs. a live restore.

## Out of scope

- **Server-side durable resume** — delivered by #54; this change only lights it up in the
  browser client.
- **Configurable/per-tenant idle-TTL policy** — #54 left this as a follow-up; not required
  here (Wander consumes whatever TTL the server runs).
- **The live-rentals MCP backend** — that is #60's scope; this change is about session
  restore-on-reload, and pairs with #60 rather than subsuming it.

## Note

Discovered from #54 while verifying durable resume end-to-end (the browser lane and README
explicitly mark the "#54 boundary"). Pairs naturally with #60 (Wander live rentals demo):
#60 lights up the data backend, this lights up refresh-to-restore. Both are Wander-example
polish, not runtime changes.
