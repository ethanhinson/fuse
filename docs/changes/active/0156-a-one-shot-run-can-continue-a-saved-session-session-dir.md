---
id: 156
slug: 'a-one-shot-run-can-continue-a-saved-session-session-dir'
title: 'A one-shot run can continue a saved session (--session DIR)'
status: 'proposed'
priority: 'high'
type: 'feat'
deliverable: 'results-artifact'
created: '2026-11-18'
updated: '2026-11-18'
depends_on: []
stacked_on:
related: []
discovered_from: []
adrs: []
spec: 'docs/superpowers/specs/2026-11-18-a-one-shot-run-can-continue-a-saved-session-session-dir-design.md'
plan:
results:
auto_groomable:
hold:
branch_prefix:
branch:
pr:
blocked_by:
reconciled: false
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-11-18-a-one-shot-run-can-continue-a-saved-session-session-dir-design.md](../../superpowers/specs/2026-11-18-a-one-shot-run-can-continue-a-saved-session-session-dir-design.md) |
<!-- docket:artifacts:end -->

## Why

fuse is one process per job, and since the server tier was removed (317ab835) nothing can continue a conversation: a one-shot run writes no history, and fuse shell keeps its history in memory behind a terminal UI. A program that holds a long conversation with fuse (the factory's orchestrator, which talks a brief through with a person turn by turn) has had to rebuild the history into each task file by hand. That rebuilt context drifted from what happened: it put later messages and the current message itself into the history, and the orchestrator told the person "this is the same message you sent before". fuse already holds the conversation it ran, compaction included; it only has to keep it for the next run.

## What changes

- `runtime.LoopConfig` gains `History`: earlier turns the root agent runs on top of, before the new task.
- `fuse --session DIR` (one-shot): loads the conversation saved in DIR (a versioned JSON file with explicit fields), runs the new task on top of it, and on a successful run saves the whole conversation back atomically. A failed or stopped turn saves nothing, so the session never holds a half turn.
- One turn at a time: a second run on a session in use is refused (an OS file lock, released if the process dies).
- The saved conversation is what the model saw, compaction summaries included; the system prompt is not saved (it comes from the config each run).
- README documents multi-turn headless use.

## Out of scope

- A long-lived process or server: fuse stays one process per job.
- Resuming `fuse shell` sessions, or rebuilding a conversation from events.jsonl.
- Compaction of long user/assistant text beyond what summarization already does (it compacts tool output).
- Archiving one-shot compaction segments (one-shot installs no segment sink today).
