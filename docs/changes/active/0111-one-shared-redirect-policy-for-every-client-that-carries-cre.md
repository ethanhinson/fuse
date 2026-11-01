---
id: 111
slug: 'one-shared-redirect-policy-for-every-client-that-carries-cre'
title: 'One shared redirect policy for every client that carries credentials'
status: 'proposed'
priority: 'medium'
type: 'fix'
deliverable: 'pr'
created: '2026-11-01'
updated: '2026-11-01'
depends_on: []
stacked_on:
related: [102]
discovered_from: [102]
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

MCP clients, research providers and web_fetch each decide redirect handling separately; credentials or secret headers can follow a redirect to another host in paths not covered by the loop-scoped secret fix.

## What changes

- One CheckRedirect policy (refuse cross-host or scheme-downgrade hops when a credential is attached) shared by MCP HTTP clients, research providers and web_fetch.
- Audit every caller and add redirect tests.

## Out of scope

- Address pinning (see change 105).
