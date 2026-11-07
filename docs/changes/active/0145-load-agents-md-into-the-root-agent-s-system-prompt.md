---
id: 145
slug: 'load-agents-md-into-the-root-agent-s-system-prompt'
title: 'Load AGENTS.md into the root agent''s system prompt'
status: 'proposed'
priority: 'high'
type: 'feat'
deliverable: 'pr'
created: '2026-11-07'
updated: '2026-11-07'
depends_on: []
stacked_on:
related: []
discovered_from: []
adrs: []
spec:
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
<!-- docket:artifacts:end -->

## Why

AGENTS.md is the standard way to give a coding agent project instructions, and fuse does not read it. Repo conventions cannot reach a run today.

## What changes

- At the start of a one-shot run or shell session, include AGENTS.md from the working directory in the root agent's system prompt.
- Subagents do not receive it.
- A missing or empty file changes nothing.
- A very large file is truncated with a visible note.
- Tests prove the root and child prompts through a real request.

## Out of scope

CLAUDE.md fallback, nested per-directory files, and imports.
