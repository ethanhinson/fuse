---
name: model-compat-layer-for-tool-loops
description: Schema-stripping compat layers cause more problems than they solve — use only a fingerprint loop detector; Cline and Grok Build validate this approach
metadata:
  type: feedback
  change: 1
  created: 2026-08-04
  updated: 2026-08-04
  promotion_state: candidate
  changes: [1, 2]
  topics: [compat, tool-use, local-models, loop-detection]
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
