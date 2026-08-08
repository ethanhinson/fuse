<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0028 — Semantic tool-result relevance scoring for smarter pruning](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0028-semantic-tool-relevance.md)**
<!-- docket:backlink:end -->

# Semantic tool-result relevance scoring — results

Change: #0028 · Branch: feat/semantic-tool-relevance · Plan: docs/superpowers/plans/2026-08-08-semantic-tool-relevance-plan.md · ADRs: 15

## Verify (human)

Automated tests fully cover the behavior; no manual checks are strictly required. Optional smoke check at the merge gate:

- [ ] With `context.relevance.classifier_model` unset (default), confirm a real over-budget session prunes as before (heuristic-only, recency floor intact) — no behavior change is expected versus pre-0028 for the recency-floored half.

## Findings

- **Plan deviation / ADR-0015 — per-result vs batched classification.** The spec called for the hybrid classifier to *batch* borderline candidates (`classifier_batch_size`, default 10). The `RelevanceScorer` interface is per-result (`Score(r, ctx)`) and the prune scores candidates one at a time, so the live path issues a single-result classifier call per borderline candidate. The multi-candidate batch machinery (`classify([]ToolResult)`, batch-size, defensive multi-line parsing) is fully implemented and unit-tested, retained as the seam for a future batched-prune path, but is not yet driven by the prune. Cost is bounded chiefly by borderline-band gating (clear-cut results never reach the model), not yet by batching. Recorded as **ADR-0015** (Accepted). A follow-up could make the prune batch-aware to exercise `classifier_batch_size` live.

- **Keyword-overlap is exact-token.** The tokenizer keeps `/`, `.`, `:` inside tokens, so `internal/agent/relevance.go` and `internal/agent/relevance` are distinct tokens and do not overlap. Overlap therefore fires on exact path/identifier reuse (the common "the model named the same file it read" case), which is the intended precision. Fuzzy/substring path matching is deliberately out of scope (the classifier carries reference nuance).

- **No-op degeneration proven, not just asserted.** `TestPruneNoOpDegeneration` runs the relevance prune with a pure-recency scorer against a byte-for-byte oracle (the retained pre-0028 recency walk) across a budget × floor matrix ({0,1000,1500,2000,3000,5000} × {0,50,100}) and asserts identical stub sets and freed-token counts — the safety invariant the whole design rests on.

- **Gateway-seam verification (learning `verify-tool-loop-at-gateway-seam`).** Two `cmd/fuse` tests drive the real binary over an over-budget history against a scripted httptest gateway: one asserts the classifier request fires for the borderline band at the real seam; the other asserts a gateway failure falls the run back to the heuristic ranking with the newest result still protected (no stubbing regression). The failure test also confirms the bounded-retry path runs (the `bound-every-model-call` learning).

## Follow-ups

- **Batched-prune classifier path** — make `pruneOldToolResults` (or a two-pass wrapper) collect the whole borderline band and call the classifier once via the existing `classify([]ToolResult)` machinery, so `classifier_batch_size` is exercised live (see ADR-0015). Not filed as a stub (auto-capture disabled this run); a human may file it at the groom gate.

## Build-role degradation note

All `superpowers:*` role skills (plan, build, review, finish) are not installed on this machine, so per docket's Skill-layer missing-skill rule each role ran inline (auto fallback) with a prominent warning rather than via the configured skill. The plan was authored inline, the build ran inline with disciplined TDD (focused test → minimal implementation → package tests → self-review → commit, per task), the whole-branch review was performed inline (surfacing the ADR-0015 finding), and the PR is opened inline without merge.
