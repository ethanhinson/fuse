---
id: 24
slug: eventstore-independent-of-segment-store
title: EventStore is independent of the segment store — events born plaintext, segments untouched
status: Accepted
date: 2026-08-09
supersedes: []
reverses: []
relates_to: [17, 18, 19, 20]
change: 43
---

## Context

Change #43 (runtime-eventstore-seam) introduces a typed loop `Event` stream with a
JSONL-backed EventStore (`<baseDir>/<sessionID>/events.jsonl`) carrying FULL payloads at
every loop boundary: full assistant responses, full tool args/results, and spawn results.
This overlaps the existing segment store (ADR-0020: gzip-archived pre-summary message
regions), which also persists transcript content.

The spec flagged this as a must-resolve architectural interaction, with two coherent
resolutions:

- **(a)** events become the single complete record, and segments become a derived,
  compressed view over events; or
- **(b)** events carry full payloads but stay independent, and the segment store is left
  entirely untouched.

## Decision

Resolution **(b)**: the EventStore and the segment store are **INDEPENDENT**.

`events.jsonl` is born **PLAINTEXT** and append-only — it inherits ADR-0020's
non-destructive philosophy but NOT its gzip-at-creation. The segment store — its
born-compressed archive, its `SweepOldSegments` bridge, `segment_read`, and ADR-0020 — is
left byte-for-byte untouched. The transient content overlap (both may hold summarized-away
transcript regions) is accepted deliberately.

Rationale: making segments a derived view over events (resolution (a)) is a large, risky
refactor of the PROVEN non-destructive gzip store, and is explicitly NOT the de-risking
work of this change's additive Stage A. It would break the green-by-construction property
that the two dependent changes — #44 (spawn-handle-async) and change 3
(runtime-interface-and-binding) — rely on. Keeping them independent lets the event seam
land additively now; a future change may dedupe the overlap (e.g. events referencing
segment storage for heavy content) once the seam is proven.

## Consequences

- (+) Stage A stays fully additive and green-by-construction; the segment store and
  ADR-0020 need no change and carry no regression risk.
- (+) The event stream is a self-contained, replayable record (Replay cursor) without
  depending on the segment store's format.
- (−) Transient storage overlap: summarized-away transcript content can live in both
  `events.jsonl` (plaintext) and the gzip segment archive. Accepted; a documented
  follow-up may dedupe.
- (−) `events.jsonl` is uncompressed, so a very long session's event log grows faster than
  the gzip segments; gzip/rotation of `events.jsonl` is left as a documented follow-up (the
  on-disk JSONL format leaves room for it, ADR-0017-style).
