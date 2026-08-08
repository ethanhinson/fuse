<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0028 — Semantic tool-result relevance scoring for smarter pruning](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0028-semantic-tool-relevance.md)**
<!-- docket:backlink:end -->

# Implementation plan — Semantic tool-result relevance scoring for smarter pruning (#0028)

**Change:** [#0028](../../changes/active/0028-semantic-tool-relevance.md) · **Spec:** `docs/superpowers/specs/2026-08-08-semantic-tool-relevance-design.md` · **Date:** 2026-08-08

> **Plan-authoring note (degradation).** The configured plan skill `superpowers:writing-plans`
> is not installed on this machine, so per docket's Skill-layer missing-skill rule this plan was
> authored inline (auto fallback) by the implementer. Same for the build/review/finish roles —
> all superpowers skills are absent here; each role runs inline with a prominent warning.

## Goal

Make `pruneOldToolResults` in `internal/agent/loop.go` **relevance-aware**: reserve a guaranteed
recency floor, then fill the remaining protection budget with the highest-relevance tool results
across all ages, stubbing the rest. Ship the always-on **heuristic scorer** plus the optional,
gated, fully-bounded **hybrid LLM classifier**. Preserve every existing invariant, and guarantee
the **no-op degeneration** property: with recency-only weighting the algorithm protects exactly
the newest `protectTokens` — byte-identical to today.

## Reconcile-confirmed anchors (from the change's Reconcile log)

- The seam is `pruneOldToolResults(messages, protectTokens)` (loop.go:82). #0027 did not move it;
  the summarize pass runs *before* it (loop.go ~292) and falls through to it (loop.go:340, and
  the recovery path loop.go:370).
- `ScoreContext.Query`, `RecentArgs`, and per-result `Turn` are **derivable from `messages`**
  (user/assistant/tool boundaries; tool args by matching `ToolCallID` → assistant `ToolCall.ID`).
  No need to thread the loop's `turn` counter.
- Config follows the `ContextConfig`→`SummarizationConfig` + `rawContextConfig` mirror + `Default()`
  pattern in `internal/config/schema.go`.
- `model.Message`: `Role`, `Content`, `ToolCalls[]{ID,Name,Arguments}`, `ToolCallID`, `Name`.
- Agent-wiring mirrors the summarizer: an unexported `relevanceScorer` field + a setter, but unlike
  the summarizer the **default is non-nil** (the heuristic scorer is always-on).

## Design decisions locked for the build

1. **`ToolResult.Args` source.** Args are the assistant `ToolCall.Arguments` whose `ID` equals the
   tool message's `ToolCallID`. Build a `map[toolCallID]arguments` in a single forward pass, then
   pair each `role=="tool"` message to it. When no pairing exists (older histories), `Args=""`.
2. **`Turn` source.** Turn number = count of `role=="user"` messages at or before the message's
   index (0-based). Computed in the same forward pass. `CurTurn` = the max turn (latest user).
3. **`Query`.** The `Content` of the last `role=="user"` message.
4. **`RecentArgs`.** The `Arguments` of assistant tool calls in the most recent N turns (N=2 by a
   named constant), newest-first — the dependency signal source.
5. **Scorer default.** `New()` installs `defaultHeuristicScorer()`; `pruneOldToolResults` never sees
   a nil scorer. `context.relevance.heuristic: false` installs a **pure-recency scorer** (the no-op
   degeneration path), NOT a nil.
6. **Signature.** `pruneOldToolResults(messages, protectTokens)` gains no new *parameters* — it
   reads `a.relevanceScorer` via a method receiver. Refactor it to a method
   `(a *Agent) pruneOldToolResults(...)` OR pass the scorer + derived context in. **Chosen: keep it
   a free function but add `scorer RelevanceScorer` + derived `ScoreContext` params**, so its
   existing unit tests stay drivable without an Agent. The two call sites in `Run` pass
   `a.relevanceScorer` and a `ScoreContext` derived from `messages`. `summarizationRegion` and
   `isProtected` are untouched (they compute the floor boundary; the floor reuses that same walk).
7. **Recency floor reuse.** The floor is `protectTokens * recency_floor_pct / 100`. Protecting the
   newest results up to the floor reuses the existing newest→oldest walk (the same logic
   `isProtected` encodes). The relevance fill then runs over the remaining candidates with the
   remaining budget (`protectTokens - floorSpent`).

## Tasks

Each task is TDD: write the failing focused test first, implement minimally, run the package tests,
self-review, commit. Package: `internal/agent` unless noted.

### Task 1 — Tokenizer + `RelevanceScorer` interface + types (`relevance.go`)

- New file `internal/agent/relevance.go` in package `agent`.
- Add `RelevanceScorer` interface, `ToolResult`, `ScoreContext` structs exactly per spec §interface.
- Add the shared **tokenizer**: splits on non-`[A-Za-z0-9_/.:-]` runs, lowercases, drops tokens
  `< 3` chars and a small stopword set; returns the token slice. A `distinctive(token) bool`
  helper: contains `/`, `.`, or `:`, or matches an identifier shape.
- **Tests** (`relevance_test.go`): tokenizer splits/lowercases/drops-short/drops-stopwords;
  `distinctive` classifies path/line-ref/identifier vs plain word. Table-driven.
- Commit: `feat(0028): relevance scorer interface, types, and shared tokenizer`.

### Task 2 — Heuristic scorer (`relevance.go`)

- `heuristicScorer` implementing `Score`. Named package-constant weights:
  tool-type base table (`read_file` 0.5, `grep`/`search` 0.4, `bash` 0.3, `list_directory` 0.15,
  unknown 0.3); keyword-overlap weight; signal-keyword boost; dependency-reuse weight; recency
  weight (top-weighted). Weighted sum clamped to `[0,1]`.
- Single tokenization pass per result feeding overlap (signal 2) and dep-token capture (signal 4).
- `body_scan_bytes` cap applied to the result body prefix before tokenizing.
- `defaultHeuristicScorer()` constructor with the spec defaults; a `recencyOnlyScorer` (weights
  reduced to recency-only) for the `heuristic: false` / degeneration path.
- **Tests:** each signal in isolation (tool-type ordering; overlap fraction; error/TODO boost;
  dep-reuse bumps a result whose distinctive token reappears in a later call's args; recency decay
  monotonic). Combined-score determinism (same inputs → same output). Scores stay in `[0,1]`.
- Commit: `feat(0028): heuristic relevance scorer with five weighted signals`.

### Task 3 — ScoreContext derivation from messages (`loop.go` or `relevance.go`)

- `deriveScoreContext(messages) ScoreContext` and `pairToolResults(messages) []ToolResult`
  (or a combined `scorableToolResults`) implementing decisions 1–4: toolCallID→args map, turn
  numbering, latest query, recent args.
- **Tests:** crafted histories assert correct `Query`, `CurTurn`, per-result `Turn` and `Args`
  pairing (including an unpaired tool message → `Args==""`), and `RecentArgs` window.
- Commit: `feat(0028): derive relevance ScoreContext and paired tool results from history`.

### Task 4 — Relevance-aware `pruneOldToolResults` (`loop.go`)

- Refactor `pruneOldToolResults` to: (1) reserve the recency floor; (2) score remaining candidates;
  (3) sort by score desc, recency (turn) desc tiebreak; (4) fill remaining budget; (5) stub the
  rest. Return freed tokens (unchanged contract). Only `role=="tool"`, non-stubbed messages touched;
  tool pairing untouched.
- Thread `scorer` + `ScoreContext` params; update the two call sites in `Run` (loop.go:340, :370)
  to pass `a.relevanceScorer` and the derived context.
- **Tests** (extend `loop_test.go`):
  - **No-op degeneration** (the required invariant): with `recencyOnlyScorer` and
    `recency_floor_pct` such that the floor≥budget (or a pure-recency weighting), the exact set of
    stubbed messages is identical to the pre-change function over the same histories.
  - **Rescue**: an important old `read_file` (high overlap with the query) survives while a trivial
    recent `list_directory` is stubbed, given a budget that forces a choice.
  - **Floor never violated**: the newest results up to the floor are always protected regardless of
    score.
  - **Freed-token accounting** unchanged for the recency-only case.
- Commit: `feat(0028): relevance-aware pruneOldToolResults with recency floor + reallocation`.

### Task 5 — Config surface (`internal/config/schema.go` + loader)

- Add `RelevanceConfig` (`Heuristic bool`, `RecencyFloorPct int`, `BodyScanBytes int`,
  `ClassifierModel string`, `ClassifierBatchSize int`, `BorderlineLo/Hi float64`) to `ContextConfig`.
- Add `rawRelevanceConfig` mirror (`Heuristic *bool` so an omitted key keeps the `true` default,
  mirroring summarization's `Enabled *bool`; other fields plain scalars).
- Wire into `Default()` with spec defaults (heuristic true, floor 50, body 2048, model "",
  batch 10, lo 0.30, hi 0.60) and the tighten/merge path used by summarization.
- **Tests** (`loader_test.go`): absent `context.relevance` ⇒ all defaults; `heuristic: false`
  takes effect; explicit overrides parse.
- Commit: `feat(0028): context.relevance config block with defaults`.

### Task 6 — Agent wiring: default heuristic + scorer install (`agent.go`)

- Add unexported `relevanceScorer RelevanceScorer` field. `New()` sets `defaultHeuristicScorer()`.
- `SetRelevanceScorer(RelevanceScorer)` (nil ⇒ keep/install default), mirroring `SetStripSpawn`.
- An exported `ConfigureRelevance(cfg config.RelevanceConfig)` (or equivalent) that installs the
  recency-only scorer when `Heuristic==false`, otherwise the heuristic built from cfg. Classifier
  wiring in Task 7.
- **Tests:** `New()` yields a non-nil scorer; `Heuristic:false` ⇒ pure-recency behavior end-to-end
  through the prune (ties Task 4's degeneration test to the config path).
- Commit: `feat(0028): default-on heuristic scorer wiring on Agent`.

### Task 7 — Hybrid LLM classifier (gated, bounded, fail-safe) (`relevance.go`)

- `classifierScorer` wrapping the heuristic: heuristic ranks all; only borderline-band results
  (`[lo,hi]`) are batched (`ClassifierBatchSize`) to the classifier `Completer`; clear-cut results
  skip the call. Prompt = current query + each candidate `(toolName, args, capped result prefix)`;
  parse defensively; unparseable/partial ⇒ heuristic score for the affected items.
- **Boundedness** per the `bound-every-model-call` learning: per-attempt timeout, response-header
  timeout, bounded retries, distinct trace label `relevance-classifier`, and a repeated-failure
  suppression flag so a failing classifier cannot hot-loop across prunes. **Any** failure/timeout ⇒
  fall back to the complete heuristic ranking (never worse than recency).
- Wire `ClassifierModel != ""` ⇒ install the classifier scorer (decorate the Completer with the
  trace label at the wiring site, mirroring `EnableSummarization`).
- **Tests:** unit — only borderline band is classified; batch shape correct; a classifier returning
  error/timeout/garbage falls back to the heuristic ranking with no stubbing regression; suppression
  arms after repeated failure. Use a scripted Completer double.
- Commit: `feat(0028): hybrid borderline-only LLM relevance classifier, bounded and fail-safe`.

### Task 8 — Gateway-seam integration verification (per `verify-tool-loop-at-gateway-seam`)

- If a scripted `LLM_GATEWAY_URL` integration harness exists for #0027's summarizer, add an
  analogous test: point `classifier_model` at a scripted gateway double, drive the real over-budget
  history through the loop, assert the borderline band (and only it) is classified, the batch shape
  is correct, and a double returning error/timeout falls the run back to the heuristic ranking with
  no stubbing regression. If no such harness exists in-package, cover the seam via the Task 7
  Completer-double integration test at the `Run` level and note it in the results/PR body.
- Commit: `test(0028): gateway-seam verification of the relevance classifier path`.

### Task 9 — Full-suite gate + docs touch

- Run `go build ./...` and `go test ./...` (or the repo's suite). Fix any fallout.
- If a context-management design doc references recency-only pruning
  (`docs/designs/context-management.md`, referenced at loop.go:26), add a short note that pruning is
  now relevance-aware within the same ceiling. Optional; skip if it drifts scope.
- Commit any residual as `chore(0028): full-suite gate green`.

## Testing summary (the spec's required tests)

- **Heuristic pure-function** unit tests per signal + combined (Tasks 1,2).
- **No-op degeneration**: recency-only ⇒ byte-identical stub set (Task 4, reinforced Task 6).
- **Prune selection** table tests: old important rescued over trivial recent; floor never violated;
  freed-token accounting unchanged (Task 4).
- **Classifier via gateway seam**: borderline-only classified; failure falls back with no
  regression (Tasks 7, 8).

## Risks / watch-items

- **Determinism**: the heuristic must be deterministic (map iteration order must not leak into the
  score — accumulate into ordered slices or sums, never range-over-map for score-affecting order).
- **Tool pairing**: the prune must still only stub `role=="tool"` content; never drop/reorder
  messages. Reuse the existing stub mechanism.
- **Floor math edge**: `recency_floor_pct` of 100 ⇒ pure recency (degeneration); 0 ⇒ pure relevance
  but the score's top-weighted recency component still protects the newest — assert both extremes.
- **Classifier cost**: borderline-only batching is the cost guard; verify clear-cut results never
  reach the model.
