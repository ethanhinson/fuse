---
name: cross-deferred-integration-gap
title: Track cross-task deferrals as explicit obligations — two tasks can each defer the same wiring to the other
promotion_state: candidate
changes: [17]
created: 2026-08-06
updated: 2026-08-06
topics: [workflow, planning, subagents, integration]
---

When a per-task build worker reports "X deferred to task N by design", the dispatcher must record X as an explicit obligation on task N's brief and re-verify at the final gate that every recorded deferral was discharged. In change 0017, Task 7 deferred classifier CLI wiring to Task 9 ("that's the entry-point task") and Task 9 deferred it back to Task 7 ("the constructor is unexported — that's gate territory"), so the LLM classifier was never constructed anywhere: auto mode shipped with its probabilistic layer silently absent, failing closed on every gray-area call. It was caught only because Task 9's worker flagged the orphaned deferral in its report notes.

**Why:** Sequential workers each see only their own brief; a deferral is an inter-task edge that no single worker owns. Without the dispatcher carrying the edge forward, "deferred by design" and "dropped" are indistinguishable in every individual task report — each report looks locally complete and green.

**How to apply:** Maintain a deferral ledger across a multi-task build: every "deferred to task N" line in a worker report gets appended verbatim into task N's dispatch brief, and the pre-review gate greps the ledger for entries no later task claimed. An undischarged deferral is a gap task, not a nit.
