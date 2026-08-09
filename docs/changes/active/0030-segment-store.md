---
id: 30
slug: segment-store
title: Segment store — pre-compaction transcript archive for replay
status: in-progress
priority: low
type: feat
created: 2026-08-06
updated: 2026-08-09
depends_on: [27]
related: [27, 29]
discovered_from: [27]
adrs: []
spec: docs/superpowers/specs/2026-08-08-segment-store-design.md
plan:
results:
trivial: false
auto_groomable:
branch: feat/segment-store
pr:
blocked_by:
reconciled: false
claimed_at: 2026-08-09T03:34:37Z
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-08-08-segment-store-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-08-08-segment-store-design.md) |
<!-- docket:artifacts:end -->

## Why

Anchored summarization (change 0027) compresses old tool results into a structured summary, reclaiming context space. But compression is lossy: the full text of the summarized region is gone. If the model needs to recall an exact detail from a summarized section, it must trust the summary or re-run the tool. A **segment store** — a persistent archive of pre-summarization transcript content saved alongside the session log — gives the model (and the human) a recovery path for lost details. It also enables trace replay: a developer can load a segment store and re-run a session with different model settings or modified prompts, comparing outcomes. The design is documented in `docs/designs/context-management.md` as part of Tier 2 — this change implements the storage layer.

## What changes

Implement the concrete `SegmentSink` that #0027 leaves as a no-op seam, so summarization
becomes lossless-with-recovery. Design detail is in the linked spec; at proposal altitude:

- **Filesystem sink** persisting each pre-summarization region under a **per-session directory**
  (`~/.fuse/sessions/<session-id>/segments/`, keyed by the root agent node id), as one markdown
  file per compaction (raw region + replacing summary + metadata) plus a grep-cheap
  `index.json`. This requires **widening #0027's `SegmentSink` seam** to pass an explicit turn
  range + savings metadata (a `SegmentRegion` struct) — #0027's spec is amended to record it.
- **Per-session directory layout** — introduces a session-id (the root agent node id) and moves
  the JSONL log under `~/.fuse/sessions/<session-id>/`; existing flat logs stay read-compatible.
- **`segment_read(turn_range, tool_filter?)` built-in tool** — on-demand recovery of raw
  pre-summary content for a turn range, optionally filtered by tool name; output-bounded.
- **TUI** — a "segments archived" indicator in the detail header and an `s` "show original"
  action reusing the existing event drill-in pane.
- **GC** — a 14-day, symlink-safe sweep of segment files (parameterizing the existing session
  log sweeper), keeping the footprint bounded.
- **Replay foundation** — segments + session log carry enough to reconstruct the transcript up
  to summarization; the `fuse replay` command itself is a follow-up.

## Out of scope

- A `fuse replay` command — this change only stores the data; the replay tool is a follow-up.
- Segment encryption — file-system permission protection is sufficient for local use.
- Segment compression — raw markdown is fine; the storage cost is modest (typically KBs per session).
- Migrating existing flat logs into the per-session directory layout — old logs stay flat.
- Cross-session `segment_read` — a session reads only its own segments.

## Coordination

- **Depends on #0027** (anchored summarization), which is `proposed`/specced but not yet built.
  This change widens #0027's `SegmentSink` seam; #0027's spec is amended to record the widened
  `SegmentRegion` struct. The implementer's reconcile pass must re-validate the seam against
  merged reality at #0030 build time.
