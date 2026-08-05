---
id: 30
slug: segment-store
title: Segment store — pre-compaction transcript archive for replay
status: proposed
priority: low
type: feat
created: 2026-08-06
updated: 2026-08-06
depends_on: [27]
related: [27, 29]
discovered_from: [27]
adrs: []
spec:
plan:
results:
trivial: false
auto_groomable:
branch:
pr:
blocked_by:
reconciled: false
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
<!-- docket:artifacts:end -->

## Why

Anchored summarization (change 0027) compresses old tool results into a structured summary, reclaiming context space. But compression is lossy: the full text of the summarized region is gone. If the model needs to recall an exact detail from a summarized section, it must trust the summary or re-run the tool. A **segment store** — a persistent archive of pre-summarization transcript content saved alongside the session log — gives the model (and the human) a recovery path for lost details. It also enables trace replay: a developer can load a segment store and re-run a session with different model settings or modified prompts, comparing outcomes. The design is documented in `docs/designs/context-management.md` as part of Tier 2 — this change implements the storage layer.

## What changes

- **Segment storage** in `internal/session/` (alongside the existing JSONL logger): when the summarization pass (change 0027) compresses a region of tool results, the pre-compression content is written to a segment file:
  - Path: `~/.fuse/sessions/<session-id>/segments/<turn>-<seq>.md`
  - Content: the full tool-result transcript of the summarized region, plus the summary that replaced it, plus metadata (turn number, tool names, token count before/after).
- **Segment index**: a `segments/index.json` file per session mapping turn ranges to segment filenames, with metadata (tool count, token savings, timestamp).
- **Recovery tool**: a new `segment_read(turn_range, tool_filter?)` built-in tool that retrieves the original pre-summary content for a given range of turns, optionally filtered by tool name. The model can call this when it needs details the summary didn't capture.
- **TUI integration**: the agent tree overlay shows a "segments" indicator when summarization has occurred (e.g. "3 segments archived — 12KB compressed to 1.2KB"). The detail pane offers a "Show original" action for summarized regions.
- **GC integration**: segment files older than 14 days are swept by the existing session log GC (the `SweepOld` mechanism in `internal/session/`).
- **Trace replay foundation**: the segment store + session log provide enough data to reconstruct the full agent transcript up to the point of summarization. A future `fuse replay` command could load a session replay from these artifacts.

## Out of scope

- A `fuse replay` command — this change only stores the data; the replay tool is a follow-up.
- Segment encryption — file-system permission protection is sufficient for local use.
- Segment compression — raw markdown is fine; the storage cost is modest (typically KBs per session).

## Research notes (input for the brainstorm)

The segment store turns the summarization pass from a lossy operation into a lossless one with a recovery path. The key design insight is that the model rarely needs the full pre-summary content — most of the summarization savings come from tool results the model never re-references. The segment store is insurance for the cases where it does need them. The `segment_read` tool is intentionally not automatic: requiring the model to explicitly request the original content prevents it from re-reading everything on every turn (which would defeat the purpose of summarization). The index JSON structure (`{segments: [{turn_start, turn_end, tools: [...], token_savings, path}]}`) is designed to be cheap to query — the model can grep the index for a tool name to find which segment contains the content it needs. The 14-day GC window matches the session log sweep window, keeping the filesystem footprint bounded.
