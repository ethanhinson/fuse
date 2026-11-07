---
id: 151
slug: 'task-container-image-and-entrypoint'
title: 'Task container image and entrypoint'
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

The factory model is one container per task, but no image exists now and the default sandbox image has no language tools. A live run had to install Python over the network.

## What changes

- A documented image with fuse, git and common language tools, and a separate minimal base to extend per stack.
- An entrypoint takes a repo URL, commit and instructions, clones, runs fuse, then pushes a branch and writes the report.
- A sandbox setting documents host handler use inside the container.
- An end-to-end test on a small repo with a fake model.

## Out of scope

The job runner and the merge queue.
