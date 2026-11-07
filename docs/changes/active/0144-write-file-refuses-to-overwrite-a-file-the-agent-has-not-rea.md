---
id: 144
slug: 'write-file-refuses-to-overwrite-a-file-the-agent-has-not-rea'
title: 'write_file refuses to overwrite a file the agent has not read'
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

A live run overwrote a 91-line CLAUDE.md with 4 lines through write_file without reading it. Tests still passed, so an unattended run would ship the damage. write_file overwrites any path with no check.

## What changes

- write_file refuses to replace an existing file unless the same run has read it in full or last wrote it, and the refusal tells the model to read first.
- Creating a new file stays unchanged.
- edit_file gets the same stale check: refuse if the file changed on disk since the agent last read it.
- State is per agent, so a child's reads do not authorize its parent or siblings.
- Tests cover new file, unread overwrite, read then overwrite, overwrite after an outside change, and children.

## Out of scope

Shell redirects inside bash, which are the permission layer's concern.
