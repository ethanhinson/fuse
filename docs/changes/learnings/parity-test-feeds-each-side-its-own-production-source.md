---
name: parity-test-feeds-each-side-its-own-production-source
slug: parity-test-feeds-each-side-its-own-production-source
title: A byte-equivalence test between a re-derived output and the original must feed each side its OWN real production source, never a shared synthetic input
hook: "Testing that a re-derived output (a projection, a replacement writer) is byte-identical to the original it will replace? Feed each side its own real production source — a shared synthetic input makes the test blind to the exact divergence it exists to catch."
promotion_state: candidate
changes: [43]
created: 2026-08-10
updated: 2026-08-10
topics: [testing, equivalence, projection, refactoring, go, timestamps]
---

When you build a new writer/projection that must reproduce an existing output **byte-for-byte** — so that the equivalence is the gate for later deleting the original — the test proves nothing if it hands **both** sides the *same* synthetic input. Byte-equivalence over a shared input only checks that the two code paths format an identical input identically; it is silent about any place where the two sides draw from **different production sources** that diverge in real runs. The whole point of the parity gate is to catch that divergence, and a shared-input test is structurally blind to it.

**Why it's easy to miss:** the test is green, the diff looks like a faithful projection, and the shared input feels like the clean, controlled way to compare two formatters. The gap is invisible because each side's *source* is elided — you fed them the answer. The failure only surfaces in production, where the original and the re-derived output each pull from their own real source.

**Rule:** A parity/equivalence test that gates a future deletion must reconstruct **each side's real production source independently** and compare the two — not compare two formattings of one shared value. If side A stamps `time.Now()` (local) and side B stamps UTC, the test must feed A a local timestamp and B the UTC timestamp *that B would actually produce*, then assert equality — surfacing that B needs a `.Local()` conversion to match. Ask, per field that crosses the seam: "in production, do both sides get this value from the same place, or two places that can differ?"

**How to apply:** Any time a change introduces "write X in parallel and prove it matches the existing X so we can delete the old writer," audit the test for a shared synthetic fixture feeding both sides. Replace it with two independently-sourced values (the real clock, the real serializer, the real path) so the test exercises the actual production divergence the gate is supposed to guard.

## War story

### 2026-08-10 — the projected log that "matched" only because the test fed both sides UTC (#43, PR #46)

Change 0043 (Runtime EventStore seam) re-expressed the session log as a **consumer** of the new event stream: a subscriber wrote a parallel `session.projected.jsonl` via `projectEventToLog`, and byte-equivalence with the shipped `session.jsonl` was the explicit gate for a trivial follow-up that deletes the direct `Logger.Write`. The first equivalence test fed the same UTC timestamp to both sides and passed — but in production the shipped direct write stamps `time.Now()` (**local**) while the event store stamps `TS` in **UTC**, so the projected log would have diverged in the timestamp field on every real run. Caught in the whole-branch review (SHOULD-FIX #1), fixed in `782c7d2`: `projectEventToLog` now converts `e.TS` back to `.Local()`, and the test was rewritten to compare the two **real** production TS sources instead of a shared UTC value. The blind spot was not a wrong line — it was a test that had been handed the answer.
