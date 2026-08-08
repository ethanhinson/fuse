---
id: 27
slug: context-summarization
title: Anchored context summarization at compression threshold
status: proposed
priority: high
type: feat
created: 2026-08-06
updated: 2026-08-08
depends_on: [12]
related: [12, 28, 29, 30]
discovered_from: [12]
adrs: []
spec: docs/superpowers/specs/2026-08-08-context-summarization-design.md
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
| Spec | [2026-08-08-context-summarization-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-08-08-context-summarization-design.md) |
<!-- docket:artifacts:end -->

## Why

fuse's context management (change 0012) implements two-tier pruning: at 85% of the context window, old tool results are replaced with stubs; if the provider still rejects, a hard prune retries with a quarter protection budget. This is effective but lossy — stubs discard the content of old tool results entirely. An **anchored summarization** pass at the compression threshold would replace groups of old tool results with an LLM-generated summary structured as Objective / Details / State / Next / Files. This preserves the semantic content of the pruned region at a fraction of the token cost (typically 5-10× compression), and the structured format lets the agent understand what happened without re-running tools. The design is already documented in `docs/designs/context-management.md` as Tier 2 — this change implements it.

## What changes

- **Summarization pass in the agent loop** (`loop.go`): at the existing 85% over-budget point,
  run an LLM summarization pass over the candidate region (tool results older than the
  recency-protected tail) **before** the existing stub pruning. Summarize first, prune the raw
  region, inject the summary at the protected-region boundary — sequence matters.
- **ODSNF summary template** (Objective / Details / State / Next / Files), the structured format
  from the design doc that produces actionable summaries.
- **Anchored (incremental) summaries**: the previous summary is passed back and updated in
  place, so one living O/D/S/N/F document sits at the boundary rather than summaries stacking up.
- **Bounded summarizer**: its own per-attempt timeout, response-header timeout, bounded retries,
  a distinct trace label, and a capped output — an unbounded model call would be a silent stall.
  Model is configurable (`context.summarization.model`; empty = main model).
- **Robustness**: an input ladder (drop oldest turns → strip tool outputs) so the summarizer
  call cannot itself overflow, and a suppression state so repeated summarizer failures cannot
  hot-loop. Any failure falls back to today's stub pruning — additive, never regressive.
- **`SegmentSink` seam**: a no-op interface where the raw pre-summarization region would be
  persisted. **Persistence itself is deferred to change 0030**, which implements the sink; the
  summary's "grep your past at `<path>`" recovery pointer lights up only once a real sink is
  wired.
- **Config surface**: new `context.summarization` block in `internal/config/schema.go`
  (`enabled` / `model` / `threshold` / `max_output`).

## Out of scope

- **Segment persistence** — the storage layer is change 0030; this change ships only the no-op
  `SegmentSink` seam.
- **Relevance-based candidate selection** — recency selector only; relevance is change 0028.
- **Read-file dedup pre-pass** — that is change 0029.
- Continuous summarization (every N turns) — threshold-triggered only.
- Summarization of user/assistant messages — tool results only.
- Cross-session summarization — the anchored summary lives for the session only.

## Design decisions

Design settled through an interactive brainstorm on 2026-08-08 and captured in the linked spec,
building on the existing `docs/designs/context-management.md` (Tier 2). Four decisions fixed the
shape: (1) segment persistence is **deferred to change 0030** via a no-op `SegmentSink` hook;
(2) both the summarizer **input ladder** and **suppression state** ship in v1; (3) summaries are
**anchored** (the previous summary is updated incrementally); (4) the default summarizer model is
the **main model** (`context.summarization.model` empty). This change anchors a four-change
cluster — 0028/0029/0030 all build on it — so the scope is kept deliberately tight.
