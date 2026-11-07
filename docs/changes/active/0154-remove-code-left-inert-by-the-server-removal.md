---
id: 154
slug: 'remove-code-left-inert-by-the-server-removal'
title: 'Remove code left inert by the server removal'
status: 'proposed'
priority: 'low'
type: 'chore'
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

PR #149 left working code nothing uses in production: tenant vocabulary, the zero sandbox principal, pool-keyed scheduler paths, hook structs, and the skills Source abstraction. It costs reading time and misleads new work.

## What changes

- Remove each item, or record why it stays.
- No behaviour change, and existing tests of kept behaviour stay intact.
- `go build`, `go vet` and `go test ./...` pass.

## Out of scope

Any new feature work.
