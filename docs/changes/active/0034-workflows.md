---
id: 34
slug: workflows
title: Workflows — skill-bound subagent pools with typed workers and spawn quotas
status: proposed
priority: high
type: feat
created: 2026-08-06
updated: 2026-08-06
depends_on: [33]
related: [24, 26, 33, 36]
discovered_from: [33]
adrs: [2, 7]
spec: docs/superpowers/specs/0034-workflows.md
plan:
results:
trivial: false
auto_groomable:
branch:
pr:
blocked_by:
reconciled: false
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [0034-workflows.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/0034-workflows.md) |
| ADRs | [ADR-0002](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0002-research-mode-skill-driven-on-subagent-runtime.md), [ADR-0007](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0007-scheduler-single-admission-authority.md) |
<!-- docket:artifacts:end -->

## Why

Skills can describe multi-agent discipline but cannot enforce it. The research skill
mandates depth-1 spawning and 4-5 children; a deepseek-flash run built a depth-3,
38-spawn tree anyway, because the rules live in prose the children never see, and a
parent structurally cannot withhold spawn_agent from a child (the child builders
unconditionally re-register it). Change 0033's global brakes are the right safety net
but the wrong altitude for per-workflow policy — its larger budget would fund the
disobedient fan-out a compliant research run never needs. Peer harnesses (Codex roles,
Grok Build roles/personas + workflow budgets, Claude Code agent types) all converged on
typed, tool-restricted workers plus scoped quotas.

## What changes

A new first-class concept, **workflow**: a named binding of an invocable skill to a
spawn policy and a worker pool, configured via a `workflows:` config block (skill
frontmatter may embed defaults; config overrides; local tightens only).

- **Pool** per workflow subtree: `concurrent` slots (reversible schema strip, a
  reservation within the global cap), `total` spawn quota (permanent strip), and
  `max_depth` (static strip) — reusing 0033's stripping machinery at workflow scope.
- **Typed workers**: named worker definitions with tool allowlists (and optional model
  pin); `spawn_agent` gains a `worker` param enumerating them. A worker without
  spawn_agent in its allowlist structurally cannot nest.
- **Folded-in fix**: child builders honor a `tools` subset that omits spawn_agent
  instead of unconditionally re-registering it (`cmd/fuse/shell.go`,
  `cmd/fuse/research_probe.go`).
- **First instance**: `/research` ships as an embedded workflow — `facet-researcher`
  worker (`web_search`, `web_fetch`, `read_file`), pool `{concurrent: 5, total: 8,
  max_depth: 1}` — and the skill text swaps unenforceable prose for worker-typed spawns.

Design detail and acceptance criteria in the linked spec.

## Out of scope

- Workflow composition/chaining (0026 composes the unit this change defines).
- Cross-workflow scheduling priorities; bash-form invocables (schema-ready only).
- Changes to the global brakes themselves (0033 lands first; this depends on it).

## Open questions

- Nested workflow activations: stack tighter-wins, or forbid in v1?
- Shared top-level worker registry once a second workflow exists?
- TUI annotation of workflow roots and pool state.

## Reconcile log
