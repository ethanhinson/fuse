---
id: 148
slug: 'show-edit-diffs-in-the-shell'
title: 'Show edit diffs in the shell'
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

The shell shows an edit as a one-line tool call, so a person cannot see what changed or judge an approval prompt.

## What changes

- edit_file and write_file results render as a compact colored diff in the shell transcript and in the approval prompt.
- Long diffs collapse with a line count.
- Tests cover rendering, collapse, and a narrow terminal.

## Out of scope

Syntax highlighting and side-by-side views.
