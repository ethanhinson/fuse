---
id: 134
slug: 'installed-skills-come-from-a-skill-source-adapter-consistent'
title: 'Installed skills come from a skill source adapter, consistent across replicas'
status: 'implemented'
priority: 'medium'
type: 'feat'
deliverable: 'pr'
created: '2026-11-03'
updated: '2026-11-06'
depends_on: []
stacked_on:
related: [86, 118, 128]
discovered_from: []
adrs: []
spec: 'docs/superpowers/specs/2026-11-03-installed-skills-come-from-a-skill-source-adapter-consistent-design.md'
plan: 'docs/superpowers/plans/2026-11-05-installed-skills-come-from-a-skill-source-adapter-consistent-plan.md'
results: 'docs/results/2026-11-03-installed-skills-come-from-a-skill-source-adapter-consistent-results.md'
auto_groomable:
hold:
branch_prefix:
branch: 'feat/installed-skills-come-from-a-skill-source-adapter-consistent'
pr: 'https://github.com/ethanhinson/fuse/pull/138'
blocked_by:
reconciled: true
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-11-03-installed-skills-come-from-a-skill-source-adapter-consistent-design.md](../../superpowers/specs/2026-11-03-installed-skills-come-from-a-skill-source-adapter-consistent-design.md) |
| Plan | [2026-11-05-installed-skills-come-from-a-skill-source-adapter-consistent-plan.md](../../superpowers/plans/2026-11-05-installed-skills-come-from-a-skill-source-adapter-consistent-plan.md) |
| Results | [2026-11-03-installed-skills-come-from-a-skill-source-adapter-consistent-results.md](../../results/2026-11-03-installed-skills-come-from-a-skill-source-adapter-consistent-results.md) |
<!-- docket:artifacts:end -->

## Why

Installed skills are read once at boot from hard-coded home directories with no config key. Replicas can advertise different skill sets, and a loop that references an installed skill can fail to resume on a replica that lacks it. Carried skills already solved this for spec-supplied skills; operator-installed skills need the same discipline.

## What changes

A `SkillSource` seam with a filesystem implementation (configurable directories) and a Postgres implementation, a periodic refresh so the server's set follows the source, and a recorded skill digest on the stream so resume can detect drift. Conformance suite; a two-server capabilities-agreement test.

## Out of scope

Skill bundle files beyond `SKILL.md` (they are not served today either); carried skills (done); a skill-management API.

## Reconcile log

### 2026-11-06

Resume after contended reconcile: reconciled against origin/main. No in-flight changes affect the skills package.

## Build evidence

<!-- docket:build-evidence:start -->
command:  make test
result:   green
head_sha: 0d7c8bee40e535815a9e36ffc75eef55c4fe41ad
ran_at:   2026-11-06T09:21:25Z
<!-- docket:build-evidence:end -->
