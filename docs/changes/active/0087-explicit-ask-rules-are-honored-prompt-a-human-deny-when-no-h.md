---
id: 87
slug: 'explicit-ask-rules-are-honored-prompt-a-human-deny-when-no-h'
title: 'Explicit ask rules are honored: prompt a human, deny when no human is bound'
status: 'proposed'
priority: 'high'
type: 'fix'
deliverable: 'pr'
created: '2026-10-31'
updated: '2026-10-31'
depends_on: []
stacked_on:
related: [86]
discovered_from: []
adrs: [5, 49, 61]
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
| Artifact | Link |
|---|---|
| ADRs | [ADR-0005](../../adrs/0005-per-segment-allow-rule-evaluation.md), [ADR-0049](../../adrs/0049-allowlist-admission-on-deterministic-allow-paths.md), [ADR-0061](../../adrs/0061-a-loop-runs-a-client-harnessspec-layered-over-the-operator-c.md) |
<!-- docket:artifacts:end -->

## Why

In auto mode a `permissions.auto.ask` or `always_prompt` match returns the same verdict as "no rule matched", so the gate falls through to the safelist and heuristic and usually allows. A rule an operator or spec wrote to force a prompt silently does nothing. Hosted loops make it worse: they bind an always-approve stand-in, so even a distinct ask would be answered yes with no person involved.

## What changes

- Explicit ask (an ask rule or always_prompt match) becomes its own verdict that skips the safelist, heuristic and classifier.
- With a human approver bound, it prompts as before.
- With a policy stand-in (hosted loop server, approve-all, headless MCP server), it denies at the rules layer with a reason naming the matched pattern.
- "No rule matched" keeps today's fallthrough.
- ADR recording the headless explicit-ask policy.

## Out of scope

- A client-visible approval protocol for hosted loops.
- Glob matching semantics (separate change).
- Safelist content or new permission modes.
