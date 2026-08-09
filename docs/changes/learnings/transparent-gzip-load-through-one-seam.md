---
slug: transparent-gzip-load-through-one-seam
title: Add compression behind one load seam by magic-byte sniffing plus a .gz path fallback so old and new files both read
hook: "To make an archive born-compressed without a migration, funnel every read through one loader that sniffs the gzip magic (0x1f 0x8b) AND falls back from a bare path to path+.gz — so pre-existing plaintext and new .gz files both load through the single seam, and every downstream reader (tool, TUI, index) inherits it for free"
topics: [go, filesystem, compression, backwards-compat]
changes: [30]
created: 2026-08-09
updated: 2026-08-09
promotion_state: candidate
---

## Apply

When you switch a persisted artifact to compressed-on-write **after** plaintext files already
exist, do not migrate and do not branch every call site. Put a single `Load`/`Open` seam in front
of all reads and give it two transparent behaviors:

1. **Sniff the content** — read the gzip magic bytes (`0x1f 0x8b`) and gunzip when present,
   otherwise return the bytes as-is. Old plaintext and new `.gz` both work.
2. **Fall back on the path** — if the requested bare path is missing, retry `path + ".gz"`. Callers
   keep asking for the logical name; the seam resolves plaintext-or-compressed.

Every downstream reader (the read tool, the TUI drill-in, the index scan) inherits compression
with zero change. Keep small must-scan index files (`index.json`) **uncompressed**. If the archival
sweep also stops deleting, make it **non-destructive** (gzip to `<path>.gz` + a metadata sidecar,
remove the original only after the `.gz` lands; no-op when a `.gz` already exists) so the operation
is idempotent and re-runnable.

## War story
- 2026-08-09 (#30, PR #41) — Segment store PR #41 follow-up made segments born-compressed
  (`<n>.md.gz`) and converted three destructive age-based sweeps (log, spill, segments) to
  non-destructive gzip-archive with YAML metadata sidecars. `segment.LoadSegment` sniffs the gzip
  magic and falls back from `.md` to `.md.gz`, so `segment_read`, the TUI show-original pane, and
  the index all inherited compression unchanged; `index.json` stayed uncompressed. An antagonistic
  answer-quality suite proved gzipped-recovered data yields the same answer as the uncompressed
  original.
