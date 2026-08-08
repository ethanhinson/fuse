<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0027 — Anchored context summarization at compression threshold](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0027-context-summarization.md)**
<!-- docket:backlink:end -->

# Anchored context summarization at compression threshold

**Change:** [#0027](../../changes/active/0027-context-summarization.md) · **Status:** design · **Date:** 2026-08-08

## Problem

fuse's context management (change 0012, `done`) is **Tier 1 — deterministic, lossy pruning**.
In `internal/agent/loop.go`, when the hybrid token estimate crosses 85 % of the context
window (`pruneThresholdPct`), `pruneOldToolResults` replaces every tool result outside the
recency-protected tail (`pruneProtectTokens = 40_000`) with the stub
`[old tool result cleared to free context — re-run the tool if needed]`. If the provider still
rejects for length, a hard prune retries once with a quarter budget; only then does the turn
error with `ErrContextTooLarge`.

This works but **throws away the content**. An old `read_file` or `grep` result that mattered
becomes a stub the model can only recover by re-running the tool. `docs/designs/
context-management.md` specs a **Tier 2 — compaction** layer to fix this: at the same 85 %
threshold, run an **LLM summarization pass** over the old region and replace it with a compact
structured summary (Objective / Details / State / Next / Files) before pruning the raw content.
Typical 5–10× compression, and the structured format lets the agent keep working without
re-running tools. **This change implements Tier 2.** It is additive and fail-safe: any
summarizer failure falls back to today's Tier-1 stub pruning, so behavior never regresses.

This change anchors a four-change cluster. **#0028 (semantic relevance scoring)** later swaps
recency-based candidate selection for relevance-based; **#0029 (read-file dedup)** adds an
optional pre-compaction dedup pass; **#0030 (segment store)** persists the raw pre-summarization
transcript for recovery/replay. All three `depends_on: [27]`. The scope boundary with #0030 is
the central design decision below.

## Design decisions (settled)

The interactive brainstorm settled four points. `superpowers:brainstorming` was unavailable in
the grooming session, so the design was reached inline with the human (docket Skill-layer
missing-skill fallback) and is recorded here as final.

### D1 — Segment persistence is deferred to #0030 via a no-op `SegmentSink` hook

#0027 owns the **summarize → prune → inject** flow and defines a clean seam where the raw
pre-summarization region would be persisted — a `SegmentSink` interface with a **no-op default
implementation**. #0027 writes nothing to disk. **#0030 implements `SegmentSink`** against
`~/.fuse/sessions/.../segments/` with replay/indexing/GC. The summary's recovery pointer
("grep your past at `<path>`") is emitted **only when a real sink is wired** — with the default
no-op sink the summary simply omits the pointer line. This keeps #0027's scope tight and hands
#0030 a defined contract instead of a shared writer.

### D2 — Input ladder and suppression state both ship in v1

Both robustness features from the design doc are in scope:

- **Summarizer input ladder** — the summarizer call is itself a model call over (candidate
  region + prompt + previous summary) and could overflow. Before giving up, shrink its input
  deterministically: **drop oldest turns → strip tool outputs from the candidate region**. Only
  if the smallest rung still cannot run does the pass fall back to Tier-1 pruning.
- **Suppression state** — after a summarizer failure, **suppress** further summarization for a
  bounded number of turns (fall back to stub pruning during suppression) so a persistently
  failing summarizer cannot hot-loop a model call every turn. Suppression clears after the
  window.

### D3 — Summaries are anchored (incremental), not single-shot

The **previous summary is passed back** into each compaction so the summarizer **updates** one
living O/D/S/N/F document rather than emitting an independent summary each time. One summary
lives at the protected-region boundary and evolves; summaries never stack up over a long
session.

### D4 — Default summarizer model is the main model

`context.summarization.model` empty ⇒ summarize with the session's main model (`a.modelID`).
Zero-config works and gives the best summary quality; a cheaper summarizer is opt-in by naming
a model in the config key. Matches the stub's stated default.

## What we build

### Trigger and sequence (`internal/agent/loop.go`)

The summarization pass slots into the existing over-budget branch in `Agent.Run`
(`loop.go:142`), **before** `pruneOldToolResults`:

```
estimate > budget:
  if summarization enabled AND not suppressed:
      region  := candidate span (tool results older than the protected tail)
      summary := summarize(ctx, region, previousSummary)   // bounded; input ladder
      if summary ok:
          sink.Archive(region)             // no-op default (#0030 implements)
          replace region with the summary message at the protected-region boundary
          previousSummary = summary
          recompute estimate
      else:
          suppress for N turns
  // fall through to existing Tier-1 pruning if still over budget (or summarization off/failed)
  freed := pruneOldToolResults(...)
  ... existing ErrContextTooLarge path unchanged ...
```

**Sequence matters** (design-doc invariant): the summarizer must see the full content, so
summarize **first**, then prune the raw region, then inject the summary at the boundary of the
protected tail. Tool-call/tool-result pairing stays valid — the injected summary is a synthetic
assistant/tool message that replaces a contiguous span, never orphaning a pair (same discipline
`pruneOldToolResults` already honors by only touching `role == "tool"` messages).

### Candidate selection

Same recency rule as Tier 1 for v1: the candidate region is tool results **older than the
recency-protected tail** (`protectBudget(window, false)` — newest ~40k tokens + recent turns).
Grouped by turn/logical boundary, never summarized individually. (Relevance-based selection is
#0028's job — this change keeps the recency selector so #0028 can swap it cleanly.)

### The summarizer (`internal/agent/summarize.go`, new)

- A **bounded model call** — this is the `bound-every-model-call` learning applied directly.
  It reuses the existing bounded transport (`internal/model/adapter.go`: `RequestTimeout`,
  `ResponseHeaderTimeout`, retry backoff, `trace`/`traceLabel`) with a **distinct trace label**
  (e.g. `summarizer`) so its REQ/RESP/RETRY/ERROR blocks are visible in the trace, a
  per-attempt timeout (default 30 s per the stub), and `max_output` capped (default 2000
  tokens). An unbounded/untraced summarizer would be a silent multi-minute stall — explicitly
  disallowed.
- **Prompt: the ODSNF template** from the design doc:
  ```
  Objective:  what the agent was trying to do
  Details:    key findings and intermediate results
  State:      current progress and open items
  Next:       planned next steps
  Files:      files touched and their state
  ```
  The **previous summary** (D3) is included with an instruction to update it in place.
- **Input ladder** (D2): if the summarizer input exceeds its own budget, drop oldest turns,
  then strip tool outputs from the candidate region, then give up (→ fallback).
- **Fallback** (fail-safe): any failure — timeout, error, empty output, ladder-exhausted —
  returns "no summary," and the caller falls through to Tier-1 stub pruning and arms
  suppression (D2). Summarization is additive; it never regresses behavior.

### The `SegmentSink` seam (`internal/agent/`, new)

```go
type SegmentSink interface {
    // Archive persists the raw pre-summarization region; returns a recovery
    // pointer (path) or "" if nothing was persisted.
    Archive(region []model.Message) (pointer string, err error)
}
```

Default: a **no-op sink** returning `("", nil)`. #0030 implements the real sink. When the
pointer is non-empty the summary carries a "grep your past at `<path>`" line; with the no-op
sink that line is omitted. `Archive` errors are best-effort — logged, never fatal to the turn.

### Config surface (`internal/config/schema.go`)

New `context.summarization` block (alongside the existing `context_window`):

```yaml
context:
  summarization:
    enabled: true       # Tier 2 on by default
    model: ""           # empty = main model (D4)
    threshold: 0.85     # context-window fraction (shares Tier-1's 85%)
    max_output: 2000    # summarizer output cap
```

Suppression window and per-attempt timeout are internal constants (mirroring
`pruneThresholdPct` / `pruneProtectTokens`), not config surface, unless a test shows they need
tuning.

## Out of scope

- **Segment persistence** — the storage layer is #0030; #0027 ships only the no-op `SegmentSink`
  seam (D1).
- **Relevance-based candidate selection** — recency selector only; relevance is #0028.
- **Read-file dedup pre-pass** — that is #0029.
- **Continuous summarization** (every N turns) — threshold-triggered only.
- **Summarizing user/assistant messages** — tool results only (keeps pairing valid).
- **Cross-session summary persistence** — the anchored summary lives for the session only.

## Tests

- **Trigger sequence**: over-budget turn invokes the summarizer **before** pruning; the raw
  region is replaced by the summary at the protected-region boundary; the estimate drops below
  budget; tool-call/tool-result pairing remains valid (no orphaned pairs).
- **Fail-safe fallback**: summarizer timeout / error / empty output falls through to existing
  Tier-1 stub pruning with **identical** post-state to today (a golden comparison against the
  pre-change prune path).
- **Suppression**: after a summarizer failure, subsequent over-budget turns **do not** call the
  summarizer for N turns (assert no summarizer request in the trace), then resume.
- **Input ladder**: an over-large candidate region drives the ladder (drop oldest turns → strip
  tool outputs) and still produces a summary; assert the rung actually taken; ladder-exhaustion
  falls back.
- **Anchoring (D3)**: two successive compactions pass `summary_v1` into the second call and
  produce an updated `summary_v2`; only one summary lives in context at a time.
- **Bounded call (learning `bound-every-model-call`)**: the summarizer carries a per-attempt
  timeout, a response-header timeout, bounded retries, and a **distinct trace label** — assert
  its REQ/RESP block is present and labeled in a scripted trace.
- **`SegmentSink`**: default no-op returns `("", nil)` and the summary omits the recovery
  pointer; a fake sink returning a path makes the pointer line appear.
- **Config**: `enabled: false` disables Tier 2 (pure Tier-1 behavior); empty `model` uses the
  main model id; a named `model` routes the summarizer call to that id.
- **Real-binary verification** (learning `verify-tool-loop-at-gateway-seam`): drive the real
  binary against a scripted `LLM_GATEWAY_URL` double that returns a summary for the summarizer
  request and asserts the post-compaction request carries the injected summary in place of the
  raw region — the loop wiring, not just a unit of `loop.go`.

## Risks & mitigations

- **Summary loses information the model needs** (the dominant risk) — mitigated by the ODSNF
  `Next`/`State` fields that preserve intent, the anchored update (D3) that carries context
  forward, and #0030's raw recovery path once it lands. v1 without #0030 accepts that the raw
  region is unrecoverable after compaction — an acceptable interim, since Tier 1 already
  discards it entirely.
- **Summarizer stall** — foreclosed by the bounded transport + per-attempt timeout
  (`bound-every-model-call`).
- **Failure hot-loop** — foreclosed by suppression state (D2).
- **Summarizer input overflow** — foreclosed by the input ladder (D2), with Tier-1 fallback as
  the floor.
- **Broken tool pairing** — the summary replaces only `role == "tool"` spans at a turn boundary;
  the same invariant `pruneOldToolResults` already upholds.

## Follow-ups (not this change)

- #0030 implements the `SegmentSink` (raw archive, replay, GC).
- #0028 swaps recency candidate selection for relevance scoring.
- #0029 adds the read-file dedup pre-compaction pass.
- Tuning suppression window / summarizer timeout to config if usage shows a need.
