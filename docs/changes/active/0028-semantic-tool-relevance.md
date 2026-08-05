---
id: 28
slug: semantic-tool-relevance
title: Semantic tool-result relevance scoring for smarter pruning
status: proposed
priority: medium
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

The current context pruning strategy (change 0012) is recency-based: the newest ~40k tokens of tool results are protected, everything older is a pruning candidate. This is simple but naive — an old tool result about a fundamental architectural decision may be far more important than a recent `ls` output. Recency-based pruning discards the important old content while keeping the trivial new content. A **relevance scoring** system that ranks tool results by their importance to the current task — using a lightweight heuristic (tool type, result size, keyword overlap with recent queries) and optionally an LLM classifier — would retain the most important content regardless of age, making much better use of the finite context window.

## What changes

- **`RelevanceScorer` interface** in `internal/agent/` (or `internal/context/`):
  ```go
  type RelevanceScorer interface {
      Score(toolName string, args string, result string, query string, turn int) float64
  }
  ```
- **Heuristic scorer** (default, always-on): a rule-based scorer that assigns higher relevance to:
  - Tool type: `read_file` > `grep` > `bash` > `list_directory` (read results are more likely to be re-read)
  - File paths matching recent `grep`/`read_file` queries (keyword overlap)
  - Error results (higher retention — model may need to debug)
  - Results with "TODO", "FIXME", or error keywords (higher relevance)
  - Tool results from the current turn's dependency chain (parents of tools that were re-called)
- **LLM classifier scorer** (optional, gated): a lightweight model call that scores a batch of candidate results for relevance to the current conversation. Gated behind `context.relevance.classifier_model` config.
- **Relevance-aware pruning**: instead of "protect newest N tokens," the pruning pass scores all tool results, sorts by relevance, and retains the top N tokens worth of results. Recency is a tiebreaker for equal scores.
- **Config surface**:
  ```yaml
  context:
    relevance:
      heuristic: true
      classifier_model: ""  # empty = heuristic only
      classifier_batch_size: 10  # results per classifier call
  ```

## Out of scope

- Embedding-based semantic similarity (requires a vector store) — heuristic + LLM classifier is sufficient.
- Per-tool custom scorers — the heuristic scorer is extensible by tool name.
- Feedback loop (model marking results as useful/useless) — deferred.

## Research notes (input for the brainstorm)

The heuristic scorer is inspired by the observation that in agent transcripts, certain tool result types are far more likely to be re-referenced: `read_file` outputs (the code itself), `grep` results (location references), and error outputs (the model needs to understand failures). The keyword overlap signal is cheap (string matching against the current user message and recent assistant tool calls) and catches the common case of "I just read a config file and now I need to recall its content." The LLM classifier scorer is the more powerful option: it takes the last user message + the candidate tool result and scores it 0-1. The design tension is latency vs. quality: the heuristic scorer is instant (<1ms) but has blind spots; the classifier adds 500ms-2s per batch but catches nuance. The hybrid approach (heuristic first, classifier only for borderline scores) is the most practical. The turn-based dependency chain tracking is a novel idea from the Grok Build codebase: if tool B's arguments reference tool A's result, tool A is higher relevance because the model may need to re-verify.
