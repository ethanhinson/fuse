---
id: 73
slug: one-shot-default-model
title: One-shot honors models.default when --model is unset
status: done
priority: medium
type: fix
created: 2026-08-20
updated: 2026-08-20
depends_on: []
related: []
discovered_from: [72]
adrs: []
spec:
plan:
results:
trivial: true
auto_groomable: false
branch: feat/one-shot-default-model
claimed_at: 
pr: https://github.com/ethanhinson/fuse/pull/78
blocked_by:
reconciled: true
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| PR | [#78](https://github.com/ethanhinson/fuse/pull/78) |
<!-- docket:artifacts:end -->

## Why

Found live while verifying change 0072 against the todo-app: `fuse "<task>"` with no `--model`
flag dies with `runtime: build agent: model "": unknown model ""` even though the config sets
`models.default: glm` — and the flag's own help text promises "(default: config default)".
`run()` passed the raw flag value straight into `runtime.LoopConfig{ModelID: *modelAlias}`
(cmd/fuse/main.go), so an unflagged run reached `BuildAgent` as model `""`. The other entry
points were already correct: `runShell` seeds its alias from `reg.Default` (shell.go:35) and
`research-probe` defaults explicitly (research_probe.go:56-58).

## What changes

One guarded assignment in `run()` after flag parsing: `*modelAlias == ""` ⇒ `reg.Default`, so all
three downstream consumers (the agent tree, `buildOneShotRuntimeDeps`, `StartLoop`) see the
resolved alias. Pinned by `TestOneShotNoModelFlagUsesConfigDefault` (scripted gateway double,
config-default model, no flag ⇒ exit 0 with the scripted reply).

## Out of scope

Early alias validation for a friendlier error on a broken registry default — `validateModelRefs`
already covers a configured-but-unknown `models.default` at startup.

## Reconcile log

### 2026-08-20

Minted retroactively at the human's direction while closing out 0072; trivial (single guarded
assignment mirroring two existing implementations), built directly.
