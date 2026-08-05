---
name: bound-every-model-call
slug: bound-every-model-call
title: Never make an LLM/API call on a default HTTP client — bound, retry, and trace every call, including children's
hook: "Every model call needs a per-attempt timeout, a response-header timeout, bounded retries, and a labeled trace entry — http.DefaultClient hangs are silent multi-minute stalls, and untraced child agents make them invisible"
promotion_state: candidate
changes: [12]
created: 2026-08-05
updated: 2026-08-05
topics: [llm, http, timeouts, retries, observability, tracing]
---

`http.DefaultClient` has no timeout. A model call that hangs (slow gateway, oversized payload, dead upstream) stalls the whole loop for minutes with zero signal. Any code path that calls a model — especially ones added later for background/child agents — must go through one bounded client:

- **Per-attempt timeout** (0012 chose 5m) and a **response-header timeout** (60s) so a dead connection fails fast while long streams still work.
- **Bounded retries with backoff** (3 attempts), cancel-aware so Ctrl-C actually stops the call.
- **Failure context**: errors carry model, payload size, attempt count, and duration — "the call failed" is undebuggable; "claude-x, 330KB, attempt 3/3, 61s" is.
- **Trace everything, including children**: subagent calls must land in the same trace (mutex-guarded, per-agent labels). 0012's hang was invisible precisely because child agents were untraced.

Two follow-on rules from the same incident family:
- A stalled call is often a symptom of an **unservable payload** (context balloon: 3k → 150k prompt tokens from verbatim tool outputs). Bounding the client makes the failure visible; fixing it needs context management (truncation/spill, pruning) upstream.
- Pattern-detect provider context-length rejections and prune-and-retry **exactly once** — retrying the same oversized payload just burns the retry budget.

## War story

(#12, PR #10) — Fuse subagent runtime, trace5: a child agent's synthesis request (8 full files in the prompt) hung on `http.DefaultClient` — no timeout, no retry, no trace entry — presenting as a silent multi-minute stall. Fixed with a shared bounded client (5m/attempt, 60s header, 3 attempts, backoff, cancel-aware), rich failure context in UI + `── ERROR ──` trace blocks, and one shared trace file with per-agent labels so children are visible. Trace6 (context balloon → LiteLLM starvation) drove the companion context-management layer: spill-file truncation, recency pruning at 85% of the window, prune-and-retry-once on provider length rejections.
