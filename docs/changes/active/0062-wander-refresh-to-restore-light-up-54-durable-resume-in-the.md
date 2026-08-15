---
id: 62
slug: wander-refresh-to-restore-light-up-54-durable-resume-in-the
title: 'Wander refresh-to-restore — light up #54 durable resume in the browser demo'
status: proposed
priority: medium
type: feat
created: 2026-08-15
updated: 2026-08-15
depends_on: [54, 60]
related: [50, 56]
discovered_from: [54]
adrs: []
spec: docs/superpowers/specs/2026-08-15-wander-refresh-to-restore-design.md
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
| Artifact | Link |
|---|---|
| Spec | [2026-08-15-wander-refresh-to-restore-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-08-15-wander-refresh-to-restore-design.md) |
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

## What changes

Design settled in [the spec](../../superpowers/specs/2026-08-15-wander-refresh-to-restore-design.md);
proposal-altitude summary. **Builds on #60's consolidated demo** (the real `@fuse/sdk` + Connect base),
not the soon-retired `examples/wander`. No runtime or SDK changes — the browser SDK already exposes
everything (`observe(loopId, {fromSeq})`, ADR-0037 terminal `not_found`, `isCompletion`).

- **Persist `loopId`** across page loads (localStorage); on load, **restore before resume** —
  `observe(loopId, {fromSeq: 0})` replays the durable transcript into the UI, then continues live,
  instead of `startLoop`. No stored id ⇒ the current fresh-session path.
- **Expiry semantics (the demo's core):** the server's 30-min idle-TTL ends the **live** session
  (composer disables, "session paused — reload to resume"), but a **reload restores the full
  conversation from durable events** (#54's rehydration). **Reap ≠ loss** — a build that starts fresh
  on mere idle expiry, throwing away restorable events, is wrong.
- **`not_found` → clean fresh session:** the only true loss is a gone durable stream (store
  wiped/rebuilt). On the SDK's terminal `not_found`, clear the stored id and start fresh silently — no
  hot-loop, no broken UI. Other terminal codes stay the demo's auth error.
- **README + CI lane:** retire the "#54 boundary" note; extend the headless-browser reconnect lane
  with a **reload** case (new page context, same persisted `loopId` → transcript restored → `Send`
  continues) — the browser-level analogue of #54's server cold-resume acceptance.

## Out of scope

- **Server-side durable resume** — delivered by #54; browser-client only, no runtime/SDK changes.
- **Configurable/per-tenant idle-TTL policy** — #54 follow-up; Wander consumes the server's TTL.
- **Durable-store event retention/TTL** — #54 did not add it; `not_found` in the demo means a
  wiped/rebuilt store.
- **The live-rentals backend + demo consolidation** — that is #60 (a dependency), not this change.

## Note

Discovered from #54 while verifying durable resume end-to-end (the browser lane and README
explicitly mark the "#54 boundary"). Pairs naturally with #60 (Wander live rentals demo):
#60 lights up the data backend, this lights up refresh-to-restore. Both are Wander-example
polish, not runtime changes.
