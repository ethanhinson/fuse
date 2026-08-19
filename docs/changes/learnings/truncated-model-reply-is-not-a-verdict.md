---
slug: truncated-model-reply-is-not-a-verdict
hook: "A fail-closed parse makes a truncated model reply indistinguishable from a considered refusal — bound max_tokens against observed usage, and emit reply-health (truncated / parse_ok / tokens) so the two are separable in telemetry."
topics: [llm, permissions, observability, fail-closed, classifier]
changes: [69]
created: 2026-08-19
updated: 2026-08-19
promotion_state: candidate
promoted_to:
---

## Apply

When an LLM's structured reply gates a decision and the parse is fail-closed, a `max_tokens` set too
low does not surface as an error — it surfaces as **the safe verdict, every time**. Empty or
truncated content parses to nothing, the guard denies/asks as designed, and the system looks like a
correctly cautious gate while actually never consulting the model at all. Any allow-bias work you do
on the prompt is silently defeated, and no test catches it because the tests assert prompt wording,
not live token budgets.

Three moves:

1. **Set the bound from observed usage, not from a guess.** Measure real completion tokens for the
   reply shape against the model you actually run, and leave headroom — reasoning-style models spend
   tokens before the JSON. A budget that fits the *output schema* is not a budget that fits the
   *reply*.
2. **Make reply health a first-class field, not an inference.** Record `truncated`, `parse_ok`,
   token counts, latency, and the model id alongside the verdict. Without it, "the classifier asked"
   and "the classifier never answered" are the same row, and the incident is unfalsifiable from the
   outside.
3. **Retain the model's rationale on every verdict, including the safe one.** Discarding the reason
   at parse time is what makes a truncation-ask and a considered-ask identical downstream.

The general shape: any fail-closed guard over a fallible producer needs a channel that distinguishes
*the producer said no* from *the producer said nothing*. Otherwise the failure mode is invisible in
exactly the direction that looks correct.

Related: [[bound-every-model-call]] (timeouts/retries around the same call),
[[fail-closed-guard-calibrate-benign-set]].

## War story

- 2026-08-19 (#69, PR #75) — Live verification of fuse's retuned web_fetch classifier found that a
  fallthrough host (`web.archive.org`, deliberately not in the known-good set) always asked. Cause:
  `classifierMaxTokens = 128` deterministically truncated deepseek-flash mid-reasoning, yielding
  empty content and a fail-closed ask for **every** fallthrough host — silently defeating the whole
  point of change 0069's allow-bias retune. Observed real usage was 101–297 completion tokens; the
  bound was raised to 512 and the same fetch then classified a clean allow. The truncation had
  shipped precisely because a truncation-ask was indistinguishable from a considered ask, so the
  in-branch follow-up made decisions audit-complete: the classifier's bounded reason is retained on
  every verdict, a `classifier` block carries `model`/`latency_ms`/token counts/`truncated`/
  `parse_ok`/`cached`, and
  `fuse_permission_classifier_replies_total{tenant_id, outcome=ok|truncated|parse_error|cached}`
  makes the failure mode a graphable metric rather than a quiet safe-looking verdict.
