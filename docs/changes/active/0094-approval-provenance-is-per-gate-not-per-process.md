---
id: 94
slug: 'approval-provenance-is-per-gate-not-per-process'
title: 'Approval provenance is per gate, not per process'
status: 'proposed'
priority: 'medium'
type: 'fix'
deliverable: 'pr'
created: '2026-10-31'
updated: '2026-10-31'
depends_on: []
stacked_on:
related: [87, 92]
discovered_from: [92]
adrs: [66]
spec: 'docs/superpowers/specs/2026-10-31-approval-provenance-is-per-gate-not-per-process-design.md'
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
| Spec | [2026-10-31-approval-provenance-is-per-gate-not-per-process-design.md](../../superpowers/specs/2026-10-31-approval-provenance-is-per-gate-not-per-process-design.md) |
| ADRs | [ADR-0066](../../adrs/0066-explicit-ask-rules-are-their-own-verdict-in-auto-and-smart-m.md) |
<!-- docket:artifacts:end -->

## Why

Approval provenance (human vs policy) is a process-wide mark while the approver is bound per gate. Child agents built with the always-approve stand-in inside a human-attended run carry human provenance, so an explicit ask rule in a child falls through to always-approve and is recorded as decided by a human. The explicit-ask rule (prompt a human, deny when none is bound) is therefore not honored for those children, and the audit trail is wrong.

## What changes

- Provenance is a property of each gate, set where its approver is bound.
- A gate whose approver is the always-approve stand-in is policy provenance regardless of how the process was started.
- Explicit ask rules in such children deny at the rules layer like any other policy-bound gate, and decisions record the right decider.
- Tests for a human-attended run with an always-approve child.

## Out of scope

- A client-visible approval protocol.
- Matching semantics.
