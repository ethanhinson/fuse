---
id: 15
slug: per-result-relevance-classification
title: Per-result relevance classification over a per-result scorer interface
status: Accepted
date: 2026-08-08
supersedes: []
reverses: []
relates_to: []
change: 28
---

## Context

Change 0028 adds semantic tool-result relevance scoring so the loop's prune keeps
the results that still matter and drops the stale ones. Its scoring seam is the
`RelevanceScorer` interface, which is deliberately **per-result**: it decides one
tool result at a time given the surrounding context. The relevance-aware prune
consults that scorer one candidate at a time while filling its relevance band.

The spec also called for an optional hybrid LLM classifier that would **batch**
the borderline-band candidates — the ones the cheap heuristics can't confidently
place — into a single model call, bounding the cost and latency of consulting a
model at all. But a per-result scorer interface offers no natural batching seam:
the prune hands the scorer candidates individually, so there is nowhere for a
"score these N together" call to attach without either changing the scorer
interface or adopting a heavier two-pass prune. We had to reconcile a per-result
scoring contract with a batch-shaped cost-control goal.

## Decision

Keep the `RelevanceScorer` interface per-result and satisfy the LLM classifier
against it as a **one-item batch**. Concretely:

- The classifier scorer implements the per-result score method by issuing a
  classifier call for a single borderline result at a time.
- Clear-cut results never reach the model at all — the heuristics place them, and
  that gating is the primary cost guard.
- Any classifier failure, timeout, or unparseable response falls back to the full
  heuristic ranking and arms a run-scoped suppression flag, so the fail-safe
  posture is unchanged from a heuristics-only prune.
- The classifier call is bounded (no tool use) per the bound-every-model-call
  discipline.
- The **multi-candidate batch machinery** — a classify path that accepts a slice
  of results, the batch-size knob, and defensive parsing of a multi-line score
  response — is fully implemented and unit-tested, but is **not yet driven by the
  live prune path**. It is retained as the seam a future batch-aware or two-pass
  prune can adopt to refine an entire borderline band in one model call, without
  reworking the per-result scorer interface.

## Consequences

- The hybrid classifier ships now with correct, fail-safe behavior, without
  changing the per-result scorer interface or committing to a heavier two-pass
  prune.
- Cost is bounded chiefly by borderline-band gating (clear-cut results never reach
  the model), **not yet** by batching. With many borderline candidates in one
  prune, the live path makes N single-item calls rather than the batched
  ceil(N / batch-size) calls the batch machinery would enable. The batch-size
  config is therefore parsed and wired, but exercised only through the retained
  batch path and its unit test — not the live prune.
- The batch machinery is deliberately kept ahead of its live caller (not
  dead-code-removed) as the extension point for a follow-up batched-prune change.
  A reviewer encountering a batch-shaped classify entry point should read it as
  intentional forward wiring, not an orphaned code path.
