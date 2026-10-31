---
id: 88
slug: 'protective-permission-patterns-match-across-slashes-and-in-e'
title: 'Protective permission patterns match across slashes and in every mode alike'
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
adrs: [5, 49]
spec: 'docs/superpowers/specs/2026-10-31-protective-permission-patterns-match-across-slashes-and-in-e-design.md'
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
| Spec | [2026-10-31-protective-permission-patterns-match-across-slashes-and-in-e-design.md](../../superpowers/specs/2026-10-31-protective-permission-patterns-match-across-slashes-and-in-e-design.md) |
| ADRs | [ADR-0005](../../adrs/0005-per-segment-allow-rule-evaluation.md), [ADR-0049](../../adrs/0049-allowlist-admission-on-deterministic-allow-paths.md) |
<!-- docket:artifacts:end -->

## Why

Permission patterns use `path.Match`, where `*` does not cross `/`. A deny rule like `bash:rm *` misses `rm -rf a/b`, and an ask rule like `bash:cat*` misses `cat dir/file`, so protective rules fail open on any argument containing a slash. Smart and auto modes also match against different subjects (first word vs whole segment), so the same rule means different things per mode.

## What changes

- Protective consumers (deny, ask, always_prompt) use a command glob where `*` spans `/`.
- Allow rules (auto_approve) keep strict `path.Match`, so no existing allow widens.
- One subject rule in every mode: patterns match per parsed segment against `bash:<argv0> <args>`, and deny/ask also against the whole command and the `bash:<argv0>` head; smart mode keeps its first-token allow match for compatibility.
- ADR: protective patterns match generously, permissive patterns match strictly.

## Out of scope

- The explicit-ask verdict and headless policy (separate change).
- Changing default rule sets.
- Regex or other new pattern syntax.
