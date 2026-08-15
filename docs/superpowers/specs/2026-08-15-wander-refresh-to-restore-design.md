<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0062 — Wander refresh-to-restore — light up #54 durable resume in the browser demo](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0062-wander-refresh-to-restore-light-up-54-durable-resume-in-the.md)**
<!-- docket:backlink:end -->

# Wander refresh-to-restore — design

Change: [#0062](../../changes/active/0062-wander-refresh-to-restore-light-up-54-durable-resume-in-the.md) ·
Depends on #54 (done), #60 · Related #50, #56 · ADRs consulted: 0033 (Connect wire), 0037 (SDK terminal-vs-transient classification)

## Problem

Change #54 landed durable, resumable sessions on the **server**: an interactive loop survives client
disconnect, its transcript is rebuilt from the durable event stream, and a reconnecting client
re-`observe`s from seq 0 (full replay) then keeps chatting via `Send` (transparent server-side
Resume). Verified end-to-end against the real `loop-serve-net` server across a real disconnect +
multi-turn conversation.

The Wander demo deliberately stops exactly at the "#54 boundary" — its own `README.md`/`app.js` say
"Wander is stateless across page loads … a refresh starts a fresh loop … without needing
durable/resumable sessions (#54)." So today a **page refresh mints a brand-new loop and the
conversation is lost**, even though the server can now restore it. This change crosses that boundary in
the browser client: refresh → restore.

## Grounding (verified at groom time)

The browser SDK already has everything this needs — no runtime or SDK changes:

- `observe(loopId, { fromSeq, onState, signal })` — history-from-`fromSeq` then live tail (the #56
  reconnect surface); `fromSeq: 0` replays the whole durable transcript.
- Terminal-vs-transient error classification (ADR-0037): `not_found` is a **terminal** code the SDK
  does NOT retry — exactly the "this stored loop no longer exists" signal.
- `isCompletion(event)` — the `loop.parked` turn-boundary marker the UI already keys on.
- Wander's `app.js` already tracks `loopId` (created lazily on first message) and a seq cursor.

## Dependency on #60

**Builds on the consolidated demo #60 produces**, not the soon-retired `examples/wander`. #60 merges
the two example apps onto the real `@fuse/sdk` + Connect `fuse.loop.v1` base (dropping concierge-demo's
pre-#55 WS proxy) and keeps the reconnect CI lane. #62's restore rides on that single consolidated
base — sequencing after #60 avoids doing the work twice on a path that is about to change.

## Decisions

### D1 — Persist `loopId` across page loads; restore-before-resume on load

Persist the active `loopId` (localStorage or equivalent) so a reload knows which session to restore.
On page load:

1. If a stored `loopId` exists → `observe(storedLoopId, { fromSeq: 0 })`: replay the durable transcript
   into the UI (render the prior messages), then continue live and re-enable the composer. Do **not**
   call `startLoop`.
2. If none stored (or the stored one resolves gone — see D3) → the current first-visit path: mint a
   fresh loop lazily on the first message.

The restore renders history from the same event kinds the live path already handles (`user.input`,
assistant/`model.call.end`, `tool.*`, `loop.parked`), so no new rendering code — it is the existing
observe handler fed the replay.

### D2 — Expiry semantics: idle ends the LIVE session; reopen restores from durable events

This is the heart of the demo and must be exact, because #54 rehydrates a *reaped* session from the
durable stream — so idle-expiry alone does **not** lose the conversation:

- **Idle-TTL reached** (the server's 30-min default; the loop is reaped, marked not-live): the **live**
  session ends. Wander surfaces this as a paused/logged-out state — the composer disables, a subtle
  "session paused — reload to resume" affordance appears. The conversation is **not** discarded.
- **Reload after idle**: `observe(loopId, { fromSeq: 0 })` triggers the server's transparent Resume —
  the transcript is rebuilt from durable events and re-parked — so the **full conversation is
  restored** and chatting continues. This is the showcase: "close the tab for an hour, reopen, your
  chat is still there."
- **Conversation truly lost only on `not_found`** (D3): the durable stream itself is gone (store
  wiped / a real deployment's retention policy aged it out — neither of which #54 added, so in the
  demo this means a cleared/rebuilt store). That is the only path to a fresh session.

The distinction the spec pins: **reap ≠ loss.** A build that starts a fresh session on mere idle
expiry (throwing away restorable durable events) is wrong — it wastes #54's whole point.

### D3 — `not_found` → clean fresh session (never a hot-loop or broken UI)

When `observe(storedLoopId, …)` returns the SDK's terminal `not_found` (ADR-0037), the stored loop is
gone: clear the persisted `loopId` and fall back to a fresh first-visit session. Silent by default (the
user sees a new chat), with an optional subtle "previous session expired" toast — never a retry loop,
never a stuck spinner. Other terminal codes (`unauthenticated`/`permission_denied`) surface as the
demo's existing auth error, not a fresh session.

### D4 — README + CI-lane coverage

- Update the consolidated demo's README to retire the "#54 boundary / stateless across page loads"
  note — Wander now crosses it.
- Extend the headless-browser CI lane (the `browser_test.go` reconnect lane #60 carries onto the
  consolidated demo) with a **reload** case: drive a turn, tear down the page context entirely (not
  just a mid-stream network drop), open a fresh page context with the same persisted `loopId`, and
  assert the transcript is restored and a further `Send` continues the conversation. This is the
  browser-level analogue of #54's server-level cold-resume acceptance.

## What changes (scope)

- The consolidated demo app (from #60): persist `loopId`; restore-before-resume on load; paused-state
  UX on idle; `not_found` → clean fresh session; README update.
- The browser CI lane: add the reload-restore case alongside the existing mid-stream reconnect case.

## Out of scope

- **Server-side durable resume** — delivered by #54; this is browser-client only, no runtime/SDK
  changes.
- **Configurable / per-tenant idle-TTL** — #54 left this a follow-up; Wander consumes whatever TTL the
  server runs.
- **Durable-store event retention/TTL** — #54 did not add it; `not_found` in the demo means a
  wiped/rebuilt store, and the spec does not introduce retention.
- **The live-rentals MCP backend and demo consolidation** — that is #60; this depends on its
  consolidated base but does not do that work.

## Open questions for the reconcile pass

- The exact persisted-state key/shape and where the paused/`not_found` UX lives in the consolidated
  app's structure — settle against #60's final layout (which app dir, which files) at build time.
- Whether the reload CI case can reuse the existing lane's harness (same server, new page context) or
  needs a second server lifecycle — a harness detail, pinned only as "new page context, same persisted
  loopId, transcript restored, Send continues."
