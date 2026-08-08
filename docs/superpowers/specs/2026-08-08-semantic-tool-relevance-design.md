<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0028 — Semantic tool-result relevance scoring for smarter pruning](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0028-semantic-tool-relevance.md)**
<!-- docket:backlink:end -->

# Semantic tool-result relevance scoring for smarter pruning

**Change:** [#0028](../../changes/active/0028-semantic-tool-relevance.md) · **Status:** design · **Date:** 2026-08-08

## Problem

fuse's context pruning (change 0012, `done`) is **purely recency-based**. In
`internal/agent/loop.go`, `pruneOldToolResults(messages, protectTokens)` walks messages
newest→oldest, accumulates estimated tokens until it reaches `protectTokens`
(`protectBudget(window, …)`, capped at `pruneProtectTokens = 40_000`), and stubs every older
`role == "tool"` message with `[old tool result cleared to free context …]`. "Protected" means
"positionally recent," nothing more.

The blind spot: **age is a poor proxy for importance.** An old `read_file` of a core source
file, or a `grep` that located the symbol the model keeps returning to, gets stubbed — while a
trivial recent `list_directory` is protected purely because it is newer. Recency-based pruning
discards important old content and keeps trivial new content.

This change introduces a **`RelevanceScorer`** so pruning retains the *most important* tool
results, not merely the newest. It does so **within the existing token ceiling** — it changes
*which* results a fixed protection budget is spent on, never how large that budget is.

### Relationship to #0027 (dependency)

`depends_on: [27]`. #0027 (anchored summarization) runs an LLM summary pass over the old region
**before** `pruneOldToolResults`, then stubs the raw region. Its spec explicitly names this
change as the one that "later swaps recency-based candidate selection for relevance-based."
The two share one seam: the candidate-selection step inside `pruneOldToolResults`. This design
composes with #0027 rather than fighting it (see **Ordering & composition** below) and inherits
its **additive, fail-safe** posture: any scorer degeneracy or classifier failure falls back to
today's recency behavior, so pruning never regresses.

## Design

### Framing — floor + budget reallocation (not full replace)

`protectBudget` **stays the ceiling**. The change is *how* that ceiling is spent:

1. **Recency floor (guaranteed).** The newest tool results — a reserved fraction of the
   protection budget (`context.relevance.recency_floor_pct`, default 50) — are always protected,
   exactly as today. This is the safety floor that prevents stubbing content the model is
   actively using, and it is what #0027 relies on when it treats the tail as sacred.
2. **Relevance-filled remainder.** The *remaining* protection budget is filled by the
   **highest-relevance** results across **all ages** — pulling important old results back out of
   the stub set, spending budget recency would otherwise have spent on merely-newer results.
3. **Recency as signal + tiebreaker.** Recency is also the top-weighted signal in the score and
   the final tiebreaker for equal scores.

**No-op degeneration invariant.** With a scorer that returns a pure recency-decay score (the
default weights reduced to recency-only), this algorithm protects exactly the newest
`protectTokens` — **byte-identical to today's behavior**. This is the safety property the whole
design rests on and is a required test (§Testing).

### `RelevanceScorer` interface

`internal/agent/relevance.go` (new file, same package as `loop.go`):

```go
// RelevanceScorer ranks a tool result's importance to the current task in [0,1].
// Higher = more worth protecting from pruning. Implementations must be
// deterministic given identical inputs.
type RelevanceScorer interface {
    Score(r ToolResult, ctx ScoreContext) float64
}

// ToolResult is the scorable unit — one "tool" role message plus its origin turn.
type ToolResult struct {
    ToolName string
    Args     string
    Result   string
    Turn     int
}

// ScoreContext is the current-conversation signal the scorer ranks against.
type ScoreContext struct {
    Query      string   // the current user message
    RecentArgs []string // args of recent assistant tool calls (dependency signal)
    CurTurn    int      // for recency decay
}
```

The Agent gains a `relevanceScorer RelevanceScorer` field, defaulting to the heuristic scorer.
`pruneOldToolResults` is refactored to consult it (see **Prune algorithm**).

### Heuristic scorer (default, always-on)

`heuristicScorer` — a pure, deterministic, single-pass function. Signals, each contributing to a
weighted-sum score clamped to `[0,1]`; weights are named package constants (tunable, testable):

1. **Tool-type base weight** — static table: `read_file` (0.5) > `grep`/`search` (0.4) >
   `bash` (0.3) > `list_directory` (0.15); unknown tool → middle default (0.3). Extensible by
   adding tool names to the table.
2. **Keyword overlap** — tokenize `ScoreContext.Query` + `RecentArgs` into a token set; score the
   fraction of overlap with the tokens of this result's **args + a capped prefix of its body**
   (`context.relevance.body_scan_bytes`, default 2048). One shared tokenization pass per result
   (§Tokenizer). Catches "I just read this config and now need its contents."
3. **Signal-keyword boost** — additive boost when the body contains error markers
   (`error`, `panic`, `failed`, non-zero exit indicators) or `TODO`/`FIXME`. The model often
   needs to revisit failures and flagged code.
4. **Bounded dependency reuse** — a result whose **distinctive tokens** (path-like and
   identifier-like tokens from its args + capped body prefix) reappear in a **later** tool's args
   scores higher: the model referenced what this result produced, so it may re-verify it.
   Implemented with one `token → latest turn seen in a result` map, cleared per user turn — **no
   cross-tool reference graph** (see *Deferred*). Reuses the §Tokenizer output; single pass.
5. **Recency component** — normalized `(CurTurn - Turn)` decay. Top-weighted; the signal that
   makes the no-op degeneration invariant hold and the final tiebreaker.

**Tokenizer** (§shared): splits on non-`[A-Za-z0-9_/.:-]` runs, lowercases, drops tokens shorter
than 3 chars and a small stopword set. "Distinctive" tokens (signal 4) are those containing a
`/`, `.`, or `:` (paths, line refs) or matching an identifier shape. Pure and allocation-bounded
by the capped body prefix. One pass per result feeds signals 2 and 4.

### LLM classifier scorer (optional, gated — hybrid)

Gated behind `context.relevance.classifier_model` (empty ⇒ heuristic only). Wraps the heuristic:

- **Heuristic runs first for every candidate** and produces a complete ranking.
- **Only borderline results are classified.** Results whose heuristic score falls in the
  borderline band (`context.relevance.borderline_lo`/`borderline_hi`, default 0.30–0.60) are
  batched (`context.relevance.classifier_batch_size`, default 10) and sent to the classifier,
  which returns a refined `[0,1]` per result. Clear-cut results (obviously important or obviously
  trivial) skip the model call entirely — latency and cost are bounded to the ambiguous middle.
- **Prompt shape:** the current user message + each candidate's `(toolName, args, capped result
  prefix)`; the model returns one score per candidate. Output is capped and parsed defensively;
  an unparseable or partial response falls back to the heuristic score for the affected items.

**Boundedness & fail-safe** (per the `bound-every-model-call` learning and #0027's precedent):
the classifier call carries its own per-attempt timeout, response-header timeout, bounded
retries, and a **distinct trace label** (`relevance-classifier`). **Any failure, timeout, or
suppression falls back to the heuristic ranking** — never to worse-than-recency behavior, because
the heuristic already produced a complete ranking. A repeated-failure suppression flag prevents
hot-looping the classifier across successive prunes in one run. Additive and non-regressive,
exactly like #0027's summarizer.

### Prune algorithm (`internal/agent/loop.go`)

`pruneOldToolResults` is refactored to relevance-aware selection while preserving its invariants
(only `role == "tool"` messages touched; already-stubbed messages skipped; tool-call pairing
untouched):

```
1. Collect candidate tool results (role == "tool", not already stubbed) with their turn + est tokens.
2. Reserve the recency floor: protect the newest results up to
   floor = protectTokens * recency_floor_pct / 100  (today's newest-first walk, bounded to the floor).
3. Score the remaining candidates (heuristic; then classify the borderline band if a
   classifier_model is configured).
4. Sort remaining by score desc, recency desc as tiebreaker.
5. Protect from that sorted list until the remaining budget (protectTokens - floorSpent) is filled.
6. Stub every candidate not protected by step 2 or step 5.
Return estimated tokens freed (unchanged contract).
```

`protectBudget` and its `recovery` quarter-budget path are unchanged — recovery mode simply
passes a smaller `protectTokens`, and the same floor+reallocation runs against it.

### Ordering & composition with #0027

When #0027 has landed, at an over-budget turn the summarizer runs **first** over the region older
than the recency tail, injects the ODSNF summary at the boundary, then `pruneOldToolResults`
runs. Relevance operates **within** `pruneOldToolResults` (steps above): it may protect a
high-relevance *old raw result*, spending budget on it. This is deliberate and consistent — the
summary is a lossy digest of the old region; relevance rescues the *few* specific old results
worth keeping in full. The recency floor guarantees the tail #0027 depends on stays intact.
Because #0028 is designed here ahead of #0027 shipping, the reconcile pass re-validates the seam
against the real post-#0027 `loop.go` at build time; if #0027's refactor moves the seam, the
prune algorithm above is re-anchored to wherever candidate selection then lives.

### Config surface

New `context.relevance` block in `internal/config/schema.go`:

```yaml
context:
  relevance:
    heuristic: true            # heuristic scorer on (default). false ⇒ pure recency (today's behavior)
    recency_floor_pct: 50      # % of protectBudget reserved for the guaranteed recency floor
    body_scan_bytes: 2048      # capped result-body prefix scanned for overlap + dep tokens
    classifier_model: ""       # empty ⇒ heuristic only; else the model id used for borderline scoring
    classifier_batch_size: 10  # candidates per classifier call
    borderline_lo: 0.30        # heuristic scores in [lo,hi] are sent to the classifier
    borderline_hi: 0.60
```

`heuristic: false` (or an all-recency weight set) yields the no-op degeneration path exactly.

## Out of scope / Deferred

- **Full cross-tool reference graph** — parsing result bodies for structured references and
  maintaining an explicit dependency graph. Bounded token-reuse (signal 4) captures the common
  re-verify case; the hybrid classifier carries reference nuance. The full graph is a separable
  follow-on (its precision matters most once structured delegation #0024 / workflow composition
  #0026 make references structured) — noted here, not built.
- **Embedding-based semantic similarity** (vector store) — heuristic + classifier is sufficient.
- **Per-tool custom scorer plugins** — the heuristic's tool-type table is extended in-code.
- **Feedback loop** (model marking results useful/useless) — deferred.
- **Changing the token ceiling** — this change reallocates a fixed budget; it never enlarges it.

## Open questions

_None blocking — resolved in brainstorm: floor+reallocation framing, args+capped-body overlap,
bounded token-reuse dep signal (full graph deferred), hybrid borderline-only classifier._

## Verification

- **Heuristic is a pure function** — direct unit tests per signal and on the combined score,
  including the **no-op degeneration** test: recency-only weights ⇒ `pruneOldToolResults`
  protects exactly the newest `protectTokens`, identical to the pre-change result.
- **Prune selection** — table tests over crafted message histories asserting *which* tool
  results survive: an important old `read_file` is rescued over a trivial recent
  `list_directory`; the recency floor is never violated; freed-token accounting is unchanged.
- **Classifier path via the gateway seam** — per the `verify-tool-loop-at-gateway-seam` learning,
  point `classifier_model` at a scripted `LLM_GATEWAY_URL` double and drive the **real binary**
  over an over-budget history; assert the borderline band (and only it) is classified, the batch
  shape is correct, and a double returning an error/timeout falls the run back to the heuristic
  ranking with no stubbing regression.
