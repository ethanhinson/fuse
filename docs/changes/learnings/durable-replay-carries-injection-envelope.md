---
slug: durable-replay-carries-injection-envelope
hook: "A durable-transcript replay delivers user turns WRAPPED in the runtime's injection envelope (\"[human message]\\n…\", internal/agent/humanmsg.go) — not the raw text the client originally sent. Any client-side filter or matcher doing exact-match against its own sent text (quiet prompts, dedup, ownership checks) silently never fires on replay; strip the envelope before matching, and test the filter against a REPLAYED stream, not just the live one."
topics: [durable-resume, replay, streaming, client-sdk, wander, envelope]
changes: [62]
created: 2026-08-17
updated: 2026-08-17
promotion_state: candidate
promoted_to:
---

## Apply

The runtime wraps every human message in an injection envelope before it reaches the durable
stream, so what a re-`observe` replays is the *enveloped* form. A client that remembers the raw
text it sent — to suppress its own quiet prompts, dedup optimistic bubbles, or attribute turns —
will exact-match against the raw form, miss every replayed occurrence, and leak both the envelope
text and the suppressed turns into the restored UI. The failure is invisible in live-stream testing
because the live path hands the client its own raw text; it only appears on replay.

Rules: strip the envelope at the replay boundary before any matching; never assume replayed
payloads are byte-identical to sent payloads; and any lane that tests a client-side filter must
exercise the reload/replay path, in the SAME browser storage context (a fresh Playwright
`BrowserContext` is storage-isolated and turns the whole restore test into a false green).

## War story

- 2026-08-17 (#62, PR #73): the review blocker's prescribed fix (suppress the Saved-panel quiet
  turn on replay) was inert until this was found — the exact-match filter never fired because
  replayed turns carried the `[human message]` envelope, and every replayed follow-up user bubble
  leaked the envelope text. Stripping at the replay boundary made the filter live; the restore
  lane deliberately reopens a page in the same context and documents why.
