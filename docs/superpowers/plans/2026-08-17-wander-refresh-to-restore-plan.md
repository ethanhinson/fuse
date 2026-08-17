<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0062 — Wander refresh-to-restore — light up #54 durable resume in the browser demo](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0062-wander-refresh-to-restore-light-up-54-durable-resume-in-the.md)**
<!-- docket:backlink:end -->

# Wander refresh-to-restore — implementation plan

Change: **0062** · Spec: `docs/superpowers/specs/2026-08-15-wander-refresh-to-restore-design.md` (on `docket`)
Base: `origin/main` @ `2341f13` · Branch: `feat/wander-refresh-to-restore-light-up-54-durable-resume-in-the`

> **Plan-role degradation:** `skills.plan` resolves to `superpowers:writing-plans`, which is not
> invocable in this harness. Per the convention's missing-skill rule the role degraded to `auto` and
> this plan was authored directly by the implementer. Artifact and stop-point are unchanged.

## What we are building

`examples/wander` today mints a brand-new loop on every page load, so a refresh loses the
conversation — the "#54 boundary" the demo's own README advertises. #54 made the server able to
rebuild a parked loop's transcript from its durable event stream. This change crosses that boundary
in the browser: **persist the `loopId`, and on load replay it instead of starting fresh.**

All four spec decisions (D1 persist+restore, D2 reap ≠ loss, D3 `not_found` → clean fresh session,
D4 README + CI lane) land in `examples/wander/`. No runtime or SDK change.

## Grounding — what the code actually does today

Verified against `origin/main` @ `2341f13`:

- `examples/wander/app.js:106` — `let loopId = null`, module scope, minted lazily at
  `app.js:443-457` (`startLoop` → `runObserve`), cleared by `resetSession()` at `app.js:553`.
  **No `localStorage` anywhere in the app.**
- `runObserve()` (`app.js:258-420`) already calls `observe(myLoopId, { fromSeq: 0n, signal, onState })`.
  **`fromSeq: 0n` is already the full-replay call** — the restore path needs no new observe options.
- Its `catch` (`app.js:403-416`) already branches on `FuseTerminalError`, but treats **every** terminal
  code identically ("Connection closed (code). Refresh to start a new session.").
- `resetSession()` (`app.js:548-580`) is the single teardown, shared by **+ New** and `switchUser()`.
- The SDK (`sdk/ts/src/index.ts`) exposes `ConnState = "connecting" | "live" | "reconnecting" | "closed"`
  and throws `FuseTerminalError` carrying a Connect `Code` for the ADR-0037 terminal set.

### Two facts that shape the design

**(1) The event stream carries user turns, but the app never renders them.** `user.input`
(`internal/event/event.go:61`, payload `{turn, content}`) exists precisely so a from-events rebuild
keeps human turns (`internal/runtime/reconstruct.go:57`). `runObserve`'s `switch` has **no
`user.input` case** — the live path echoes the user's message optimistically in `handleSubmit`
instead. So a naive replay would restore a transcript of answers with no questions. This is the
`reconstruct-from-stream-needs-every-input-emitted` learning arriving from the consuming side.

Three `user.input` shapes exist and they are **not** all real user messages:

| Emitted at | Content | Restore behavior |
|---|---|---|
| `internal/agent/loop.go:373` (seed, turn 0) | the raw `task` — i.e. the Wander preamble **plus** the user's first message | render, **preamble stripped** |
| `internal/agent/loop.go:434` (each `send`) | the injected human message verbatim | render |
| `internal/agent/loop.go:783` (deny nudge) | `"[policy] …"`, a *synthetic* user turn | **never render** |

Wander's own quiet turns (`SAVED_REFRESH_PROMPT`, `app.js:489`) are real `send`s and therefore appear
as `user.input` too — they are app-generated and must not render either.

**(2) Replay must be side-effect-free.** `isCompletion` handling (`app.js:383-401`) re-enables the
composer, focuses the input, and — if a `favorite_listing` was seen — fires `refreshSaved()`, a real
model turn. Replaying a conversation that contains a favorite would fire that mid-replay.

We need a `replaying` flag, and we can set it **deterministically without any timer**: a restored loop
is *parked and idle*, so no new event can arrive until the user sends. Therefore
**`replaying` is true from the start of a restore stream until the next `handleSubmit`.**
The Saved panel still repopulates for free, because a replayed `list_favorites` `tool.result` hits the
existing `renderSaved` branch (`app.js:342`) — no extra turn needed.

### The storage-isolation gotcha for the CI lane

The reconcile said "a second `browser.NewContext()` is the reload case." That is **wrong for
`localStorage`**: Playwright `BrowserContext`s are storage-isolated, so a fresh context starts with an
empty `localStorage` and would silently test the *fresh-session* path while looking like it tested
restore. The faithful gesture is **close the page, open a new `Page` in the SAME context** — that is
"closed the tab, reopened it," and it is the only shape that carries the persisted id. (Copying
`StorageState()` into a new context also works; it is more machinery for the same assertion.)
This must be stated in the test's comments or a future edit will "simplify" it back into a false green.

## Decisions this plan pins

- **D-A — Storage key and shape.** `localStorage["wander.session.v1"]` =
  `{"loopId": "...", "tenant": "...", "subject": "..."}`. The principal is stored **with** the id and
  the restore only fires when it matches the current credential. Wander is a multi-identity demo; a
  loop restored under a different principal would be a cross-owner `Observe` the server rejects with
  `PermissionDenied` — a confusing failure where a fresh session is the honest answer. Any parse
  failure or shape mismatch is treated as "nothing stored."
- **D-B — Terminal codes stop being one bucket.** The `FuseTerminalError` branch splits per ADR-0037:
  - `not_found` → the durable stream is gone: **clear storage, reset to a clean fresh session**,
    subtle "previous session expired" notice. (D3)
  - `failed_precondition` → the loop finished/was reaped: **paused** — keep the stored id, disable the
    composer, show "session paused — reload to resume". (D2 — *reap ≠ loss*)
  - `unauthenticated` / `permission_denied` → unchanged auth error.
- **D-C — Persist at mint, clear at reset.** Write the entry immediately after `startLoop` returns;
  clear it in `resetSession()`, which both **+ New** and `switchUser()` already funnel through. So the
  two "I want a fresh start" gestures also forget the stored session, with no new teardown path.
- **D-D — Restore renders `user.input`, live does not.** A `user.input` case is added, gated on
  `replaying` (live turns are already echoed locally), skipping `[policy] ` nudges and the
  `SAVED_REFRESH_PROMPT`, and stripping the task preamble from the turn-0 seed. The preamble becomes a
  shared constant so the builder and the stripper cannot drift.

## Tasks

Each task: focused test first where the seam is testable, implementation, verification, self-review,
one commit.

---

### Task 1 — Extract the task preamble into a shared constant

**Why first:** D-D's strip and the existing `startLoop` builder must read the same bytes; doing this
alone keeps the diff of Task 3 honest.

- `examples/wander/app.js`: add `const TASK_PREAMBLE = "You are Wander, a friendly vacation-rental concierge. First request: ";`
  near `SAVED_REFRESH_PROMPT`, and rewrite `app.js:447` to `task: TASK_PREAMBLE + text`.
- Pure refactor: no behavior change, byte-identical task string.

**Verify:** `go build ./...` is unaffected; the wander lane still builds its bundle (`build.sh`).
Confirm the constructed string is byte-identical to the previous literal.

---

### Task 2 — Session persistence helpers (storage seam)

Add, in `app.js`, a small self-contained block:

- `SESSION_KEY = "wander.session.v1"`
- `saveSession(loopId)` — writes `{loopId, tenant, subject}` from `currentUser`; every access wrapped
  in `try/catch` (a Safari private-mode / disabled-storage throw must never break the demo).
- `loadSession()` — returns `{loopId}` **only if** the stored `tenant`/`subject` match `currentUser`;
  otherwise `null`. Malformed JSON ⇒ `null`.
- `clearSession()` — removes the key, `try/catch`-wrapped.

Wire:
- `saveSession(started.loopId)` immediately after `loopId = started.loopId` (`app.js:452`).
- `clearSession()` inside `resetSession()` alongside `loopId = null`.

Expose `window.__wanderLoopId = loopId` wherever `loopId` is assigned (mint **and** restore) — the CI
lane needs to read the id without reaching into `localStorage`.

**Test:** the storage seam is exercised by the browser lane in Task 6; there is no JS unit harness in
this repo, so correctness here is carried by the lane plus review. Keep the helpers trivial enough
that this is honest — no branching beyond the match check.

---

### Task 3 — Render `user.input` during replay only

In `runObserve`'s `switch`, add:

```
case "user.input": {
  if (!replaying) break;            // live turns are echoed by handleSubmit
  let text = String(p.content || "");
  if (text.startsWith("[policy] ")) break;        // synthetic deny nudge, not a human turn
  if (text.startsWith(TASK_PREAMBLE)) text = text.slice(TASK_PREAMBLE.length);
  if (!text || text === SAVED_REFRESH_PROMPT) break;  // app-generated quiet turn
  addMessage("user", "you", text);
  break;
}
```

Add module state `let replaying = false;`, cleared in `resetSession()`. `handleSubmit` sets
`replaying = false` as its first statement (the user acting is the end of replay, and until they act
no new event can arrive on a parked loop).

Guard the replay side effects in the `isCompletion` block: while `replaying`, skip `inputEl.focus()`
and skip the `pendingSavedRefresh` → `refreshSaved()` dispatch (clear the flag without firing).
The composer **does** re-enable — a restored session must be usable.

**Verify:** covered end-to-end by Task 6; reason explicitly about the deny-nudge and quiet-turn skips
in the self-review, since neither is exercised by the lane's fixture.

---

### Task 4 — Restore on load (D1)

Add a `restoreSession()` run at startup, next to the existing `loadDemoUsers()` /`pagehide` wiring:

- `const stored = loadSession(); if (!stored) return;` (unchanged first-visit path)
- otherwise: `loopId = stored.loopId; replaying = true;` set `window.__wanderLoopId`, set the
  `loopLabel` to the same "Live loop …" text the mint path writes, mark `window.__wanderRestored = true`,
  and call `runObserve(sessionGeneration, loopId, client, sessionAbort)`.

It must run **after** `client`/`currentUser` are initialized and must not await `loadDemoUsers()` — the
restore belongs to the credential already in hand (D-A only restores a matching principal anyway).

**Verify:** Task 6's lane.

---

### Task 5 — Terminal-code split (D2 + D3)

Rewrite the `FuseTerminalError` branch (`app.js:406-412`) per D-B. Use the SDK's `Code` enum rather
than string literals if it is exported through the bundle; otherwise compare the `err.code` value the
SDK carries and name the constant locally with a comment pointing at ADR-0037.

- `not_found`: `clearSession()`, then `resetSession()`, then a subtle notice
  ("Your previous session is no longer available — starting a new one."). Set
  `window.__wanderRestoreLost = true`. **No** retry, **no** spinner.
- `failed_precondition`: keep storage; `setConn("closed")`; `setComposerEnabled(false)`; render the
  paused affordance ("Session paused — reload to resume this conversation."); set
  `window.__wanderPaused = true`.
- auth codes: today's message, unchanged.

`window.__wanderTerminal = err.code` keeps being set first in every branch — the existing lane asserts
on it.

Add the paused-state styling to `styles.css` if the affordance needs more than the existing
`addMessage("error", …)` shape; prefer reusing an existing class over inventing one.

---

### Task 6 — Browser CI lane: the reload-restore case (D4)

In `examples/wander/browser_test.go` (`//go:build browser`, Playwright-Go + headless Chromium), add a
case alongside the existing `/__cut` reconnect lane, reusing its server harness (one `fuse
loop-serve-net`, one `node server.js`, the scripted gateway double — **never** an Anthropic model):

1. Drive turn 1 in `page1`; wait for the park.
2. Read `window.__wanderLoopId`; assert non-empty.
3. **`page1.Close()`**, then `bctx.NewPage()` — the **same** `BrowserContext`, so `localStorage`
   survives. Comment this explicitly with the isolation gotcha above, so it is never "simplified"
   into `browser.NewContext()`.
4. `page2.Goto(...)`; assert `window.__wanderRestored === true`, `window.__wanderLoopId` equals the
   id from step 2, and the restored thread contains **both** turn 1's user text and its answer — the
   user-text assertion is the one that actually proves Task 3, so it must not be dropped.
5. Send turn 2 from `page2`; assert it parks and the reply renders — the conversation continues.
6. Assert `window.__wanderTerminal` is falsy throughout (a restore is not a terminal condition).

Keep the existing reconnect case untouched and passing.

---

### Task 7 — README (D4)

`examples/wander/README.md`:

- Replace the Scope paragraph (≈ lines 179-181) that says Wander is "stateless across page loads …
  without needing durable/resumable sessions (change #54)" with the new story: the demo persists its
  `loopId` and restores the conversation from the durable event stream on reload — the browser face
  of #54 — and the only true loss is a wiped store (`not_found`).
- Fix the **+ New** note (≈ lines 55-57), which currently justifies a reload as "the honest reset"
  *because* the demo is stateless. That is now false: **+ New** is the explicit fresh-start gesture
  (it clears the stored session), while a reload restores.
- Document the idle-TTL behavior: after ~30 min idle the live session pauses; reloading brings the
  conversation back.

---

## Final gate

One full-suite run at the end (`make test`, the repo's configured `finalize.test_command`), plus the
`browser`-tagged lane for the new case. The suite is the gate; per-task focused verification does not
substitute for it.

## Out of scope (restated so a worker does not drift)

No runtime, SDK, or proto change. No configurable idle-TTL, no durable-store retention, no changes to
the `/__cut` reconnect case's assertions.
