---
id: 90
slug: 'turn-and-token-budgets-are-enforced-in-hosted-interactive-lo'
title: 'Turn and token budgets are enforced in hosted interactive loops'
status: 'implemented'
priority: 'high'
type: 'fix'
deliverable: 'pr'
created: '2026-10-31'
updated: '2026-10-31'
depends_on: []
stacked_on:
related: [86]
discovered_from: []
adrs: [61, 63]
spec: 'docs/superpowers/specs/2026-10-31-turn-and-token-budgets-are-enforced-in-hosted-interactive-lo-design.md'
plan: 'docs/superpowers/plans/2026-10-31-turn-and-token-budgets-hosted-interactive.md'
results: 'docs/results/2026-10-31-turn-and-token-budgets-hosted-interactive-results.md'
auto_groomable:
hold:
branch_prefix:
branch: 'fix/turn-and-token-budgets-are-enforced-in-hosted-interactive-lo'
pr: 'https://github.com/ethanhinson/fuse/pull/111'
blocked_by:
reconciled: true
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-10-31-turn-and-token-budgets-are-enforced-in-hosted-interactive-lo-design.md](../../superpowers/specs/2026-10-31-turn-and-token-budgets-are-enforced-in-hosted-interactive-lo-design.md) |
| Plan | [2026-10-31-turn-and-token-budgets-hosted-interactive.md](../../superpowers/plans/2026-10-31-turn-and-token-budgets-hosted-interactive.md) |
| Results | [2026-10-31-turn-and-token-budgets-hosted-interactive-results.md](../../results/2026-10-31-turn-and-token-budgets-hosted-interactive-results.md) |
| ADRs | [ADR-0061](../../adrs/0061-a-loop-runs-a-client-harnessspec-layered-over-the-operator-c.md), [ADR-0063](../../adrs/0063-max-turns-is-a-per-exchange-cap-in-interactive-loops.md) |
<!-- docket:artifacts:end -->

## Why

Interactive loops set max turns to 0 unconditionally, so a spec's `max_turns` is dropped in chat loops. Token usage reaches the agent tree only through the TUI renderer, so `throughput.session_tokens` and pool token budgets never trip in any headless host. Budget settings that are accepted and recorded but not enforced are false safety.

## What changes

- In interactive loops, a set `max_turns` caps model turns per exchange (per user message). On the cap, the loop emits `loop.parked` with reason `max_turns` and parks; the conversation continues on the next message. Uncapped stays the default.
- Non-interactive loops are unchanged.
- The agent owns usage accounting through a usage sink that every binding wires to the tree node; the TUI renderer stops counting, so nothing is counted twice.
- Session and pool token ceilings then trip in every host.
- ADR for per-exchange max_turns.

## Out of scope

- Workflow pool activation (separate change; it builds on this).
- The local TUI orchestrator side-channel's turn handling.
- Cost-based budgets.

## Reconcile log

### 2026-10-31

Unchanged against origin/main; spec line references re-checked at build time.

## Build evidence

<!-- docket:build-evidence:start -->
command:  make test
result:   green
head_sha: dbb16fc656bf41c32525b3dca746ebeccc7dcc15
ran_at:   2026-10-31T18:46:17Z
<!-- docket:build-evidence:end -->
