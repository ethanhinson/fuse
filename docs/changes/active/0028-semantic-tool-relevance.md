---
id: 28
slug: semantic-tool-relevance
title: Semantic tool-result relevance scoring for smarter pruning
status: in-progress
priority: medium
type: feat
created: 2026-08-06
updated: 2026-08-08
depends_on: [27]
related: [27, 29]
discovered_from: [27]
adrs: []
spec: docs/superpowers/specs/2026-08-08-semantic-tool-relevance-design.md
plan:
results:
trivial: false
auto_groomable:
branch: feat/semantic-tool-relevance
claimed_at: 2026-08-08T22:42:24Z
pr:
blocked_by:
reconciled: false
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-08-08-semantic-tool-relevance-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-08-08-semantic-tool-relevance-design.md) |
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
