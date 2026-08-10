---
name: relocated-emission-resource-every-projected-field
slug: relocated-emission-resource-every-projected-field
title: Relocating event emission to a choke point must re-source EVERY field the projection reads — raw vs collapsed error diverges on partial-success stop paths
hook: "Moving event emission to a single choke point (a Spawner, a middleware)? The choke point re-sources each field from whatever IT has in hand — and a projection's byte-equivalence guard breaks silently on the field you didn't audit. The classic trap: the raw run error vs the caller-collapsed error, which agree on success and the ordinary error path but DIVERGE on partial-success stop paths (max-turns, loop-detected) where the collapse returns nil."
promotion_state: candidate
changes: [44]
created: 2026-08-10
updated: 2026-08-10
topics: [go, events, projection, refactoring, error-handling, byte-equivalence]
---

## Apply

When you relocate an event's emission from N call-sites into a single choke point (a `Spawner`,
a dispatcher, a middleware), the choke point necessarily **re-sources every field of that event
from whatever it holds** — not from what the old call-sites held. If a *projection* of that event
stream is being held byte-equivalent to a still-shipping direct writer (the parity gate that lets
you later delete the direct writer), the equivalence breaks on any field whose new source diverges
from the old one in production — and it breaks **silently**, because the happy-path and
ordinary-error-path tests still pass.

The specific trap that keeps recurring: **raw error vs collapsed/validated error.** A caller
typically collapses a child's outcome into a lenient partial-success value — e.g. a max-turns or
loop-detected stop returns a `"[stopped: …]"` *string* with a **nil** `error`. The old direct
writer logged the **raw** run error and selected `kind:"error"`; the relocated choke point, holding
only the collapsed result, selects `kind:"done"`. They agree on clean success and on hard failures;
they diverge exactly on the partial-success stop paths.

**Rule:** Per field the relocated event carries, ask "does the choke point have the SAME source the
old site had, or a downstream-collapsed one?" Where they can differ, thread the **raw** value to the
choke point explicitly (a dedicated sink allocated by the choke point, mirroring any existing
per-child sink idiom) so the projection field matches the direct write, while the model/control path
keeps the collapsed value. Add a regression test that exercises the **nil-swallowing stop path**
specifically — the earlier error tests never do, because they feed a real error and never hit the
collapse's nil branch.

**How to apply:** Any change of the form "move emission to a choke point AND keep a projection
byte-equivalent to a direct writer" must, before merge, enumerate every field the projection reads
for its output (especially the discriminant it uses to pick `kind`/type) and confirm each is sourced
identically. A plan's byte-identity guard that reasons only about the obvious payload field (the
`Result` string) but not the discriminant (`Err`) is incomplete — the discriminant is the field most
likely to diverge because it is derived, not copied.

## War story

### 2026-08-10 — the projected log picked `kind:"done"` where the direct write picked `kind:"error"` (#44, PR #47)

Change 0044 (spawn handle-async) relocated `spawn.start`/`spawn.done` emission out of the three
cmd-site child builders (0043) into the single `agent.Spawner` choke point. `spawn.done` now
re-sourced its `Err` field from `childResult`'s **collapsed** error — which is `nil` on the
max-turns / loop-detected stop path (childResult returns a `"[stopped: …]"` partial-success string).
So the projected session log selected `kind:"done"` while the still-shipping direct `sessLog.Write`
(raw `rerr`) selected `kind:"error"`, breaking the byte-equivalence that is 0043's whole purpose and
the gate for the trivial follow-up that deletes the direct write. The plan had flagged the
`Result`-source subtlety but its byte-identity guard reasoned only about `Result`, not `Err` — the
discriminant. Caught in whole-branch review, fixed in `b6a7654` with a spawner-allocated
`RunErrSink` (mirroring the existing `expectsSink` idiom): each child builder reports the RAW
`a.Run` error, and the Spawner's `spawn.done` uses it for `Err`, while `SpawnDone.Err` (the
handle/model control path) stays the collapsed value. Regression test
`TestSpawnerSpawnDoneUsesRawErrOnStopPath` added, plus the missing coverage the reviewer noted (the
earlier error tests never exercised childResult's nil-swallowing branch).
