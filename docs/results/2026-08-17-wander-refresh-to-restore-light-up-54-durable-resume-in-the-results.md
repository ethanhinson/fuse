<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0062 — Wander refresh-to-restore — light up #54 durable resume in the browser demo](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0062-wander-refresh-to-restore-light-up-54-durable-resume-in-the.md)**
<!-- docket:backlink:end -->

# Wander refresh-to-restore — results

Change **0062** · Branch `feat/wander-refresh-to-restore-light-up-54-durable-resume-in-the` · Base `origin/main` @ `2341f13`
Plan: `docs/superpowers/plans/2026-08-17-wander-refresh-to-restore-plan.md` · ADR produced: **ADR-0046**

## What shipped

`examples/wander` no longer mints a fresh loop on every page load. It persists the active `loopId`
and, on load, re-`observe`s it from seq 0 — replaying the durable transcript into the UI and then
continuing live — which is change #54's server-side durable resume made visible in a browser.

Delivered against the spec's four decisions:

- **D1 restore-before-resume** — the stored loop is observed on load instead of `startLoop`.
  `observe()` was already called with `fromSeq: 0n`, so the replay itself needed no new SDK surface.
- **D2 reap ≠ loss** — an idle reap does not discard the conversation. See the correction below: the
  outcome holds, but the mechanism is not the one the spec imagined.
- **D3 `not_found` → clean fresh session** — the stored id is cleared and a fresh session starts
  silently, with no retry loop and no stuck spinner. Now covered by a positive CI lane.
- **D4 README + CI lane** — the "#54 boundary" claim is retired and the headless-browser lane gained
  a reload-restore case.

## Things the human should know

### 1. The spec's "Session paused" affordance was fiction — and the code now says so

D2 specified that the server's ~30-minute idle reap would surface as a paused UI ("session paused —
reload to resume"), keyed on a terminal `failed_precondition`. Review challenged this and a
worker verified it independently against the server: `internal/loopconnect/observe.go` has exactly
four error returns and **`FailedPrecondition` is not among them**. A reap closes the subscription
channel, the handler returns `nil` — a *clean* stream end, which the SDK classifies as transient and
re-opens from the watermark. The registry record survives on disk, so the re-observe succeeds, and a
`Send` after a reap transparently `Resume`s.

So **the reap is invisible to the user**: the conversation survives, which is what D2 actually cares
about, but there is no paused state to see. The README now describes the real behavior. The
`failed_precondition` branch was **kept and relabelled as defensive** rather than deleted — ADR-0037
lists that code in the SDK's terminal set, so a future server change could start emitting it, and
deleting the branch would route it into the generic "start a new session" arm and silently discard a
restorable session.

### 2. Replay was leaking the runtime's injection envelope

Fixing the review's blocker uncovered a second, unreported half of the same defect. A `send` reaches
the durable stream wrapped in the runtime's injection envelope (`"[human message]\n…"`,
`internal/agent/humanmsg.go`), so the client-side exact-match filter for the app's own quiet
Saved-panel prompt never fired on replay: **both** halves of the quiet turn rendered, and the
envelope text leaked into every replayed follow-up user bubble. The replay now strips that envelope;
without the strip, the blocker's prescribed fix would have been inert.

### 3. Restore was silently dead for every principal except `dev`

The persisted entry always carried the *minting* principal, but the picker selection itself was not
persisted, so `currentUser` was always the built-in dev credential at load and any non-dev session
could never match. The fix persists the principal's **name** and re-resolves its token from the demo
directory before the match guard runs — which is the decision ADR-0046 records.

**Accepted limitation:** a session started under a **pasted custom token** is not persisted and does
not restore. Persisting it would mean writing a bearer token to `localStorage`, which is precisely
what ADR-0043 exists to resist in example apps. Documented in the demo's README.

## Manual checks worth doing at the merge gate

The automated lanes cover the happy restore, the lost-loop disposition, and the composer lock. These
are the things a human eye is better at:

1. **The actual gesture.** Run the demo (`examples/wander/run.sh`), have a short conversation, close
   the tab, reopen it. The transcript should come back with your questions *and* the answers, in
   order, and the composer should be briefly locked and then usable.
2. **Save a listing, then reload.** This is the path that produced the blocker. There should be no
   orphaned concierge bubble — no answer without a question above it.
3. **Switch users, converse, reload.** Confirm the non-dev principal's conversation restores, and
   that **+ New** still gives a clean fresh session.
4. **Paste a custom token and reload.** Confirm it starts fresh (the documented limitation) rather
   than failing in some confusing way.

## Follow-ups (not filed as changes — `auto_capture` is disabled for this repo)

- **The browser lanes now carry two copies of the harness.** `startWanderBrowserStack` was added for
  the restore lanes, but `TestWanderBrowserReconnectNoLossNoDup` still has its own inline ~55-line
  copy. Pointing the reconnect lane at the shared helper was deliberately left out of a batched
  minor fix; it is a small, worthwhile refactor.
- **ADR-0046's load-bearing safety property is held only by prose.** The ADR rests on the equivalence
  "every principal storage can name is one the picker already hands out on a single click." Nothing
  asserts that in a test, and the design stops holding the moment the demo gains a principal that
  storage can name but the UI does not freely offer.
- **`handleSubmit`'s catch does not special-case `FuseTerminalError`.** A genuine
  `failed_precondition` from a `Send` whose `Resume` failed would render as the generic "Failed to
  reach the concierge". Reachable in principle (a loop live on another instance), untested, and
  outside this change's scope.

## Verification

Full suite (`make test`) green at branch HEAD, twice: once at the executed plan, and once after the
in-branch review fixes. The `//go:build browser` wander lanes were additionally run green by the
workers throughout — all against the scripted gateway double, never a real model provider.

Four fixes carry mutation evidence (the guard was watched failing before it was trusted): the
principal-match check, the orphan-bubble guard, the composer lock, and the lost-loop disposition. The
reload-restore lane also proved that a *fresh* Playwright `BrowserContext` would have made the whole
restore test a false green, since contexts are storage-isolated — the lane reopens a page in the
**same** context, and says so in a comment for exactly that reason.
