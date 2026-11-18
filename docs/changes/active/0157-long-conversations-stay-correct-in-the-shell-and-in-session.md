---
id: 157
slug: 'long-conversations-stay-correct-in-the-shell-and-in-session'
title: 'Long conversations stay correct in the shell and in --session runs'
status: 'in-progress'
priority: 'high'
type: 'fix'
deliverable: 'results-artifact'
created: '2026-11-18'
updated: '2026-11-18'
depends_on: []
stacked_on:
related: [156]
discovered_from: []
adrs: []
spec: 'docs/superpowers/specs/2026-11-18-long-conversations-stay-correct-in-the-shell-and-in-session-design.md'
plan:
results: 'docs/results/2026-11-18-long-conversations-stay-correct-in-the-shell-and-in-session-results.md'
auto_groomable:
hold:
branch_prefix:
branch: 'fix/long-conversations-stay-correct-in-the-shell-and-in-session'
pr:
blocked_by:
reconciled: false
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-11-18-long-conversations-stay-correct-in-the-shell-and-in-session-design.md](../../superpowers/specs/2026-11-18-long-conversations-stay-correct-in-the-shell-and-in-session-design.md) |
| Results | [2026-11-18-long-conversations-stay-correct-in-the-shell-and-in-session-results.md](../../results/2026-11-18-long-conversations-stay-correct-in-the-shell-and-in-session-results.md) |
<!-- docket:artifacts:end -->

## Why

A review of conversation handling found ten defects that break long conversations: a compaction summary can land between tool results of one batch (providers reject it), an earlier summary is lost on the next run, plain text is never compacted so a long chat gets stuck at ErrContextTooLarge, tool results are stubbed without being summarized, token accounting restarts every run, a failed --session run drops the whole turn, relevance scoring uses the wrong turn and query, queued steering messages are stranded, context-length detection matches unrelated errors, and a shell turn cannot be stopped without losing the conversation.

## What changes

- Compaction keeps tool pairing and adjacency valid, carries the living summary across runs, summarizes exactly what it stubs, and can compact whole old exchanges of text.
- Token estimates include the system prompt and tool schemas and carry across runs.
- A failed --session run saves its valid partial history with a marker.
- Relevance recency is indexed by assistant step and the query is the last real user message.
- Queued steering messages are drained on run exit and cleared on /clear.
- Context-length errors are matched tightly, with a floor on a learned window.
- The shell gets /compact and Esc stops a turn while keeping the conversation.

## Out of scope

- New compaction strategies beyond what the findings require.
- Changes to the event schema beyond what the fixes need.
