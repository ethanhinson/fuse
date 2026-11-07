---
id: 149
slug: 'resume-a-previous-shell-session'
title: 'Resume a previous shell session'
status: 'proposed'
priority: 'medium'
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

Closing the shell loses the conversation. The reconstruction code went with the server tier, so this needs building on the event log.

## What changes

- `fuse shell --resume [id]` restores a session's conversation from its event log, and `--resume` alone picks the latest in this directory.
- A tool call left unfinished is marked interrupted rather than rerun.
- Tests cover a round trip, an unfinished call, and a missing session.

## Out of scope

Resuming one-shot runs.
