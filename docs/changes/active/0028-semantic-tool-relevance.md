---
id: 28
slug: semantic-tool-relevance
title: Semantic tool-result relevance scoring for smarter pruning
status: implemented
priority: medium
type: feat
created: 2026-08-06
updated: 2026-08-08
depends_on: [27]
related: [27, 29]
discovered_from: [27]
adrs: [15]
spec: docs/superpowers/specs/2026-08-08-semantic-tool-relevance-design.md
plan: docs/superpowers/plans/2026-08-08-semantic-tool-relevance-plan.md
results: docs/results/2026-08-08-semantic-tool-relevance-results.md
trivial: false
auto_groomable:
branch: feat/semantic-tool-relevance
claimed_at: 2026-08-08T23:04:10Z
pr: https://github.com/ethanhinson/fuse/pull/33
blocked_by:
reconciled: true
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-08-08-semantic-tool-relevance-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-08-08-semantic-tool-relevance-design.md) |
| Plan | [2026-08-08-semantic-tool-relevance-plan.md](https://github.com/ethanhinson/fuse/blob/feat/semantic-tool-relevance/docs/superpowers/plans/2026-08-08-semantic-tool-relevance-plan.md) |
| Results | [2026-08-08-semantic-tool-relevance-results.md](https://github.com/ethanhinson/fuse/blob/feat/semantic-tool-relevance/docs/results/2026-08-08-semantic-tool-relevance-results.md) |
| PR | [#33](https://github.com/ethanhinson/fuse/pull/33) |
| ADRs | [ADR-0015](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0015-per-result-relevance-classification.md) |
<!-- docket:artifacts:end -->

## Why

fuse's context pruning today (change 0012) is purely recency-based: `pruneOldToolResults` in `internal/agent/loop.go` protects the newest ~40k tokens of tool results and stubs everything older. Age is a poor proxy for importance — an old `read_file` of a core source file, or the `grep` that located the symbol the model keeps returning to, gets discarded, while a trivial recent `list_directory` is kept just because it is newer. A **relevance scorer** that ranks tool results by importance to the current task retains the most important content regardless of age, making far better use of the finite context window — without changing how large that window is.

## What changes

- **`RelevanceScorer` interface** (`internal/agent/relevance.go`): `Score(ToolResult, ScoreContext) float64` in `[0,1]`, deterministic. The Agent defaults to the heuristic scorer.
- **Floor + budget reallocation** (not full replace): `protectBudget` stays the ceiling. A guaranteed **recency floor** (`recency_floor_pct`, default 50% of the budget) is always protected as today; the *remaining* budget is filled by the highest-relevance results across **all ages**. Recency is also the top-weighted score signal and the tiebreaker — so a recency-only scorer degenerates to today's behavior byte-for-byte (the safety invariant).
- **Heuristic scorer** (always-on, pure, single-pass): tool-type base weight (`read_file` > `grep` > `bash` > `list_directory`); keyword overlap of the current query + recent tool args against this result's args **and a capped body prefix**; a boost for error/`TODO`/`FIXME` signal keywords; and **bounded dependency reuse** — a result whose distinctive path/identifier tokens reappear in a *later* tool's args scores higher (one `token → turn` map, cleared per user turn; no cross-tool graph).
- **Hybrid LLM classifier** (optional, gated by `context.relevance.classifier_model`): the heuristic ranks everything first; only results in the **borderline band** (default 0.30–0.60) are batched to the classifier for a refined score. Clear-cut results skip the model call. The call is fully bounded (timeouts, retries, distinct trace label) and **any failure falls back to the heuristic ranking** — additive and never worse than recency, mirroring #0027's summarizer posture.
- **Relevance-aware `pruneOldToolResults`**: reserve the recency floor, score the rest, fill the remaining budget by score (recency tiebreaker), stub the unprotected remainder. All existing invariants preserved (only `role == "tool"` messages touched; tool-call pairing intact; freed-token contract unchanged).
- **Config surface**: new `context.relevance` block (`heuristic`, `recency_floor_pct`, `body_scan_bytes`, `classifier_model`, `classifier_batch_size`, `borderline_lo`/`borderline_hi`).

## Out of scope

- **Full cross-tool reference graph** (body-parsing structured references) — bounded token-reuse plus the hybrid classifier cover the case; the graph is a separable follow-on, valuable once #0024/#0026 make references structured.
- Embedding-based semantic similarity (vector store).
- Per-tool custom scorer plugins — the heuristic's tool-type table is extended in-code.
- Feedback loop (model marking results useful/useless) — deferred.
- Enlarging the token ceiling — this change reallocates a fixed budget, never grows it.

## Dependency

`depends_on: [27]` — #0027 (anchored summarization) establishes the summarize-then-prune ordering and names this change as the one that swaps recency-based candidate selection for relevance-based. This design composes with #0027 (the recency floor keeps the tail #0027 relies on intact); the reconcile pass re-validates the shared `loop.go` seam against the real post-#0027 code at build time.

## Reconcile log

### 2026-08-08

Reconciled against current `main` reality after claim; design confirmed valid, no scope changes.

- **Dependency satisfied.** #0027 (context-summarization) is archived `done`, so `depends_on: [27]` is met; #28 is build-ready.
- **Seam re-validated (the design's named build-time check).** #0027 did **not** move the candidate-selection seam. In `internal/agent/loop.go`, `pruneOldToolResults(messages, protectTokens)` still walks tool results newest→oldest by the `protectTokens` recency rule (unchanged from #0012); #0027 added `summarizationRegion`/`isProtected`/`dropPriorSummary` and a summarize pass that runs *before* the prune at the over-budget branch (loop.go ~line 292), then falls through to the unchanged Tier-1 `pruneOldToolResults` (loop.go line 340; recovery path line 370). The spec's prune-algorithm anchor (reserve floor → score rest → fill remainder) applies to `pruneOldToolResults` exactly as written. No re-anchoring needed.
- **Turn/query sourcing note (for the plan, not a scope change).** `pruneOldToolResults` currently takes only `(messages, protectTokens)`. The `ScoreContext.Query` (latest user message), `RecentArgs`, and per-result `Turn` are all derivable from the `messages` slice itself (user/assistant/tool boundaries) — the loop's `turn` counter need not be threaded through. The Agent gains the `relevanceScorer` field per spec; the prune signature is extended to consult it. This is an implementation detail the plan resolves; it does not change scope.
- **Config pattern confirmed.** `internal/config/schema.go` uses the `ContextConfig` → `SummarizationConfig` + `rawContextConfig`/`rawSummarizationConfig` mirror + `Default()` pattern; the new `context.relevance` block slots in identically.
- **#0030 (segment-store) relationship.** #0030 is a sibling build-ready candidate, not a dependency; it implements `a.segmentSink` (the archive path referenced at loop.go ~line 299). #28 operates strictly inside `pruneOldToolResults` candidate selection and does not touch the sink, so the two compose without conflict.
- **Learnings applied.** `bound-every-model-call` and `verify-tool-loop-at-gateway-seam` (both already cited in the spec's classifier + verification sections) govern the optional hybrid classifier path.
- Auto-capture disabled (`AUTO_CAPTURE_ENABLED=false`) — no follow-up stubs minted; no adjacent follow-up work surfaced this pass.
