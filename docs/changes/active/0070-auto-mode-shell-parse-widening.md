---
id: 70
slug: auto-mode-shell-parse-widening
title: Auto-mode shell-parse widening — env-prefixes, wrappers, control flow, redirects, opaque args
status: proposed
priority: medium
type: feat
created: 2026-08-17
updated: 2026-08-17
depends_on: [69]
related: [37, 40, 67, 68, 69]
discovered_from: []
adrs: []
spec: docs/superpowers/specs/2026-08-17-auto-mode-shell-parse-widening-design.md
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
| Spec | [2026-08-17-auto-mode-shell-parse-widening-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-08-17-auto-mode-shell-parse-widening-design.md) |
<!-- docket:artifacts:end -->

## Why

Stage D (final, riskiest) of the auto-mode overhaul. The shell parser fails closed → ask on shapes that dominate real agent shell usage: `FOO=1 make`, `timeout 30 go test`, `$(git rev-parse --show-toplevel)`, `> out.log`, `for`/`if` loops, `$VAR` arguments. Each is a human prompt interactively or a dead end headless. Most of these are provably safe and should be evaluated, not punted — while keeping fail-closed for what remains unprovable (opaque values must never prove path containment).

## What changes

- Benign env-prefix assignments pass (dangerous-var denylist: `LD_*`, `DYLD_*`, `PATH`, `IFS`, `BASH_ENV`, `GIT_SSH_COMMAND`, `NODE_OPTIONS`, …).
- `timeout`/`env`/`nohup` peeled with dedicated strict rules (`env` with any flag stays closed).
- Control-flow descent (`if`/`for`/`while`/`case`/blocks/subshells) into constituent simple commands.
- Literal redirect out-targets captured into `Segment.WriteTargets`, scoped through the root set; `<` and here-docs become benign.
- `Segment.Opaque` args: `$VAR` and read-only `$(…)` become opaque (unprovable ⇒ ask) instead of failing the whole command; inner substitution segments still visible to deny rules. Invariant: opaque args are NEVER resolved as paths for containment.

## Out of scope

- Any rules/classifier/web_fetch behavior change (#0068/#0069). New wrapper support beyond timeout/env/nohup (`xargs`, `docker`, `sudo`, `npx` stay fail-closed).

## Open questions

<!-- none — spec is settled from the approved plan; adversarial review focus is the opaque-arg invariant -->

## Reconcile log

<!-- Appended by docket-implement-next's reconcile pass: dated entries of what changed. -->
