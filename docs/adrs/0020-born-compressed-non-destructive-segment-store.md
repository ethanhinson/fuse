---
id: 20
slug: born-compressed-non-destructive-segment-store
title: Segments are born gzip-compressed with an uncompressed index, and age sweeps compress rather than delete (non-destructive GC)
status: Accepted
date: 2026-08-09
supersedes: []
reverses: []
relates_to: [17, 18, 19]
change: 30
---

## Context

Change #0030's original design had two properties that a scope review flagged as a self-inflicted correctness risk:

1. **The age-based sweeps were destructive.** `session.SweepOld` (`os.Remove` of old `*.jsonl` logs), `session.SweepOldSegments` (`os.Remove` of old `*.md` segments, then index prune), and `tools.sweepSpillDir` (`os.Remove` of old spill `*.txt`) all *deleted* data. But the whole point of the segment store — and the spill file, and the session log — is that they are a durable cache the model can recover from later (`segment_read`, `read_file`, grep). Deleting them silently destroys recoverable answer-quality once a horizon passes.
2. **Segments were stored uncompressed.** Rendered regions are highly compressible (repeated JSON tool output) and can be large.

The requirement is: instead of deleting, compress to gzip and keep a self-describing metadata sidecar, and *prove* (with adversarial answer-quality tests using real zips, including some made by the system `gzip` binary) that data recovered from a gzipped archive yields the SAME answer as the uncompressed original — no degradation.

Two sub-decisions were non-obvious once "compress, don't delete" was accepted: **when** to compress (at creation vs. at sweep) and **whether** the index should also be compressed.

## Decision

**Segments are born compressed at creation time, not at sweep time.** `FSSegmentSink.Archive` writes `RenderSegment` output through `gzip` to `<n>.md.gz`. `segment.LoadSegment` gunzips transparently (sniffing the gzip magic `0x1f 0x8b`) and falls back from a bare `.md` path to `<path>.gz`, so old plaintext segments and new compressed ones load through one seam; `segment_read`, the TUI show-original drill-in, and the index inherit compression with no other change. The lossless JSON raw-region format inside the file is untouched — gzip wraps the whole rendered file.

**`index.json` stays uncompressed.** It is small and is scanned on every `segment_read` to resolve overlapping turn ranges; compressing it would add a decompress to the hot recovery path for no meaningful space win.

**The age sweeps compress, never delete (non-destructive GC), and there is no second destructive horizon.** Session logs and spill files are born plaintext, so their sweeps gzip them (with domain metadata sidecars) at the current horizon via a shared `internal/archive` helper. Segments are *already* born `.md.gz`, so `SweepOldSegments` is repurposed as a back-compat bridge that only retroactively compresses *legacy* plaintext `.md` in place and re-points the index Path — it never deletes a segment and never prunes the index for a merely-compressed segment. Data is retained indefinitely, merely compressed.

The compression helper (`internal/archive`) is domain-agnostic (`Archive` + `Open` + a caller-supplied `MetaFunc`) so it imports neither `session` nor `tools`, avoiding an import cycle; each caller supplies its own sidecar fields.

## Consequences

- (+) Nothing recoverable is ever destroyed by a sweep; the model's answer-quality after a horizon passes is unchanged. Proven by a five-tier antagonistic suite, including a gz-vs-plaintext end-to-end parity gate driven through the real agent loop and a system-`gzip`-binary cross-tool arm.
- (+) Substantial space savings observed on real data (session logs ~4.6x, spill ~3x, segments ~100x+) with full transparency — no caller outside the two storage seams changed.
- (+) Back-compatible: old plaintext `.md` segments and un-gzipped logs stay readable; the transparent readers sniff the magic bytes.
- (−) Recovery reads now pay a gunzip. It is cheap relative to the model round-trip and the recovery path is not hot, but it is non-zero.
- (−) Retaining indefinitely means unbounded disk growth over a very long horizon. Accepted deliberately; a separate optional long-horizon *purge* is left as a documented follow-up rather than a destructive default.
- (−) Two magic-byte-sniffing readers (`internal/archive.Open` and `segment.LoadSegment`) carry near-identical gunzip logic. They live in different packages with different fallbacks (segment has the `.md`→`.md.gz` rename tolerance; archive is generic), so they are kept separate rather than force-shared.

Relates to change #0030 and to ADR-0017 (the fssink split), ADR-0018 (per-session layout), and ADR-0019 (the sink holder).
