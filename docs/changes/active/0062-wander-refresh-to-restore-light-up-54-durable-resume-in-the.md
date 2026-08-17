---
id: 62
slug: wander-refresh-to-restore-light-up-54-durable-resume-in-the
title: 'Wander refresh-to-restore — light up #54 durable resume in the browser demo'
status: implemented
priority: medium
type: feat
created: 2026-08-15
updated: 2026-08-17
depends_on: [54, 60]
related: [50, 56]
discovered_from: [54]
adrs: [46]
spec: docs/superpowers/specs/2026-08-15-wander-refresh-to-restore-design.md
plan: docs/superpowers/plans/2026-08-17-wander-refresh-to-restore-plan.md
results: docs/results/2026-08-17-wander-refresh-to-restore-light-up-54-durable-resume-in-the-results.md
trivial: false
auto_groomable:
branch: feat/wander-refresh-to-restore-light-up-54-durable-resume-in-the
claimed_at: 2026-08-17T21:00:12Z
pr: https://github.com/ethanhinson/fuse/pull/73
blocked_by:
reconciled: true
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-08-15-wander-refresh-to-restore-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-08-15-wander-refresh-to-restore-design.md) |
| Plan | [2026-08-17-wander-refresh-to-restore-plan.md](https://github.com/ethanhinson/fuse/blob/feat/wander-refresh-to-restore-light-up-54-durable-resume-in-the/docs/superpowers/plans/2026-08-17-wander-refresh-to-restore-plan.md) |
| Results | [2026-08-17-wander-refresh-to-restore-light-up-54-durable-resume-in-the-results.md](https://github.com/ethanhinson/fuse/blob/feat/wander-refresh-to-restore-light-up-54-durable-resume-in-the/docs/results/2026-08-17-wander-refresh-to-restore-light-up-54-durable-resume-in-the-results.md) |
| PR | [#73](https://github.com/ethanhinson/fuse/pull/73) |
| ADRs | [ADR-0046](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0046-restorable-session-persists-principal-name-not-credential.md) |
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
proposal-altitude summary. **Builds on #60's consolidated demo** (the real `@fuse/sdk` + Connect base)
— which, as the 2026-08-17 reconcile confirmed, *is* `examples/wander`: #60 retired
`examples/concierge-demo` and ported its UI onto the Wander base. No runtime or SDK changes — the browser SDK already exposes
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

## Reconcile log

### 2026-08-17 — reconciled against `origin/main` @ 2341f13

Verified the spec's claims against `origin/main` (not the working tree), per the
`reconcile-verify-claims-against-origin-not-working-tree` learning. **Design holds in full; scope
adjusted on one naming point and both of the spec's open questions are now settled.**

- **#60's consolidation went the other way than the spec's wording implies.** The spec says the work
  builds on "the consolidated demo #60 produces, **not** the soon-retired `examples/wander`." In fact
  #60 retired `examples/concierge-demo` and ported its UI *onto the Wander base*: `examples/wander` is
  now the single consolidated demo on the real `@fuse/sdk` over Connect. So the target directory is
  `examples/wander/` after all. A naming re-map, not a design change — every D1–D4 decision applies
  unchanged to that directory.
- **Open question 1 (which app dir / which files) — settled.** `examples/wander/app.js` (module-scope
  `let loopId = null`, minted lazily in the first-message path, cleared on reset; the long-lived
  `runObserve()` generator; the `FuseTerminalError` branch that today renders "Refresh to start a new
  session"), plus `index.html` / `styles.css` for the paused affordance and `README.md` for D4. There
  is **no** `localStorage`/`sessionStorage` usage today — the persisted key is greenfield.
- **D1 is cheaper than the spec assumed.** `observe()` is already called with `fromSeq: 0n`, so the
  replay-then-live behavior D1 wants is the *existing* code path. The delta is genuinely just:
  persist the id, and on load with a stored id skip `startLoop` and observe that id instead.
- **Open question 2 (reload CI harness) — settled: reuse the existing lane, one server lifecycle.**
  The lane is `examples/wander/browser_test.go` under the `//go:build browser` tag, on Playwright-Go
  with headless Chromium. It already holds a `browser` handle and creates one `BrowserContext`/`Page`;
  a second `browser.NewContext()` + `NewPage()` against the same running server is the reload case, so
  no second server lifecycle is needed. Its existing `/__cut` mid-stream kill and contiguous-seq
  no-loss/no-dup assertions stay as-is alongside the new case.
- **Server side confirmed present and unchanged by this work.** `not_found` for an unknown loop comes
  from `internal/loopconnect/handler.go` (`ErrLoopUnknown` → `connect.CodeNotFound`), and the 30-minute
  idle-TTL reaper is `defaultIdleTTL` in `internal/runtime/inproc.go`, touched by each Send/Observe —
  exactly the D2 semantics ("reap ends the live session; the durable events survive"). No runtime or
  SDK change is in scope, as the spec says.
- **D4's README targets are concrete:** the "stateless across page loads … without needing
  durable/resumable sessions (change #54)" scope paragraph, and the **+ New** button note that calls a
  reload "the honest reset" *because* the demo is stateless — that second one now needs rewording too,
  since a reload will restore rather than reset.
- No related change (#50, #56) or ADR (0033, 0037) has drifted under the spec; ADR-0037's terminal set
  still carries `NotFound`, which is what D3 keys on.

Auto-capture is disabled for this repo (`AUTO_CAPTURE_ENABLED=false`); no adjacent work surfaced that
would have been minted regardless.

## Note

Discovered from #54 while verifying durable resume end-to-end (the browser lane and README
explicitly mark the "#54 boundary"). Pairs naturally with #60 (Wander live rentals demo):
#60 lights up the data backend, this lights up refresh-to-restore. Both are Wander-example
polish, not runtime changes.
