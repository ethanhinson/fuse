---
name: model-compat-layer-for-tool-loops
description: Schema-stripping compat layers cause more problems than they solve — use only a fingerprint loop detector; Cline and Grok Build validate this approach
metadata:
  type: feedback
  change: 1
  created: 2026-08-04
  updated: 2026-08-17
  promotion_state: candidate
  changes: [1, 2, 67]
  topics: [compat, tool-use, local-models, loop-detection, permissions]
---

Weaker or heavily-quantized local models can enter a hard tool-call loop on read-only tools
(read_file, list_directory): they generate exactly N tokens of tool-call JSON, receive the result,
and call the same tool again regardless of what the result says.

**Current fix (change 0002):** a single fingerprint loop detector in `agent/loop.go`:
- Collect a fingerprint (`name + args`) for every tool call in a response
- If 3 consecutive response sets have identical fingerprints → abort with `ErrLoopDetected`
- No schema manipulation, no dedup, no forced-text turns

**Do NOT strip schemas or deduplicate repeated reads.** See war story below.

**How to apply:** The `loopDetector` in `agent/loop.go` is the only loop protection. Its limit is
a constructor argument (`loopLimit`). Raise it for agents that legitimately re-check the same file
across unrelated tasks; lowering it risks false positives on normal multi-step work.

**A deterministic *system* response is not a model loop (change 0067).** The fingerprint detector
is the right primitive for a model ignoring tool *results*, but it is the wrong primitive when the
identical repeat is a rational response to the system's own deterministic reply — a permission
denial being the canonical case. The model retries verbatim because nothing in the denial told it
what to do instead, and the generic detector then kills a run that was one sentence away from
recovering. The rule: **classify the repeat before counting it.** A policy-denied repeat bypasses
the generic abort; the denial output carries a per-layer *actionable* hint ("use the web_fetch tool
instead"); after N identical denials the loop injects an explicit `[policy]` nudge as a real user
turn — and only repetition *after* the nudge ends the run. Emit the nudge into the event stream as
well as the prompt, or a resumed session replays a conversation the model never saw.

## War story

### 2026-08-04 (#2, PR #2)

Change 0001 shipped a two-signal compat layer: on the 2nd identical read-only call, strip all tool
schemas from the next request so the model is forced to produce text.

During change 0002 (TUI MVP), testing with `deepseek-flash` on "tell me about this project"
produced:

```
! aborting: model called tools despite compat intervention (no schemas sent)
! agent: tool-call loop detected
```

Root cause: once schemas were stripped, the model hallucinated tool-call invocations that were
*syntactically correct but semantically impossible* — it invented calls for tools that no longer
existed in the schema. The compat layer prevented the answer but also prevented recovery.

Researching Cline (open source) and Grok Build (open source): neither uses a schema-stripping or
result-dedup layer. Both use only a loop/repetition counter. Removed the entire compat layer;
only the fingerprint detector remained. The hallucination problem disappeared.

**Lesson:** Schema manipulation to force model behavior is fragile — the model sees a different
API surface on compat turns and may invent calls to fill the gap. Repetition counting on the
*full call fingerprint* is the correct primitive.

### 2026-08-17 (#67, PR #71)

Session logs for auto mode showed **denial amplification** as the dominant run-killer: 11
`loop.detector.trip` events, nearly all of them models retrying a permission-denied tool call
verbatim. The detector was working exactly as designed and was still wrong — the repeat was the
model's rational response to an opaque "denied" with no alternative offered, so the correct fix was
upstream of the counter, not a higher `loopLimit`.

0067 split the two cases: typed denials (`Result.Denied` / `Result.DenyLayer`, set only by the
gate) carry per-layer hints; a policy-denied repeat bypasses the generic doom-loop abort; after 2
identical denials a `[policy]` nudge is injected as a user turn *and* emitted as `user.input` so a
resume folds it back in; only post-nudge repetition ends the run. Live-verified on a cheap gateway
model asked to `curl` a URL and retry on refusal — it took two hinted denials, **stopped retrying on
its own**, and summarized cleanly, where the same shape previously killed the run.

Second-order lesson from the same change: **the gate emitted no events at all**, so prompt
frequency — the core auto-mode UX metric — was unmeasurable and the planned loosening work had
nothing to tune against. Landing the measurement substrate (`permission.decision`: allow/ask/deny,
deciding layer, mode, bounded command preview) *before* the policy changes is what makes the later
stages verifiable rather than vibes. Emit the ask **before** the approval func and the outcome
after, so a headless auto-approve binding reads as an auditable ask→allow pair instead of vanishing.
