---
id: 118
slug: 'carried-skills-a-harness-spec-may-define-a-skill-inline'
title: 'Carried skills: a harness spec may define a skill inline'
status: 'in-progress'
priority: 'high'
type: 'feat'
deliverable: 'pr'
created: '2026-11-03'
updated: '2026-11-03'
depends_on: []
stacked_on:
related: [85, 102, 104, 113]
discovered_from: []
adrs: [77]
spec: 'docs/superpowers/specs/2026-11-03-carried-skills-a-harness-spec-may-define-a-skill-inline-design.md'
plan: 'docs/superpowers/plans/2026-11-03-carried-skills-a-harness-spec-may-define-a-skill-inline.md'
results:
auto_groomable:
hold:
branch_prefix:
branch: 'feat/carried-skills-a-harness-spec-may-define-a-skill-inline'
pr:
blocked_by:
reconciled: true
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-11-03-carried-skills-a-harness-spec-may-define-a-skill-inline-design.md](../../superpowers/specs/2026-11-03-carried-skills-a-harness-spec-may-define-a-skill-inline-design.md) |
| Plan | [2026-11-03-carried-skills-a-harness-spec-may-define-a-skill-inline.md](../../superpowers/plans/2026-11-03-carried-skills-a-harness-spec-may-define-a-skill-inline.md) |
| ADRs | [ADR-0077](../../adrs/0077-carried-skills-travel-in-the-harness-spec-not-beside-it.md) |
<!-- docket:artifacts:end -->

## Why

A harness spec references skills by name only, so the only skills a loop can use are the ones installed on the server's disk by its operator. A caller that composes specs for many principals has no way to give a loop its own skill, and no way to pin which skill content a loop ran. The spec already carries other caller-authored prompt content (instructions, roles), is digested, recorded and pinnable; skills should travel the same way.

## What changes

`SkillRef` gains `description` and `body`; an entry with a body is a carried skill the loop runs as carried, shadowing an installed skill of the same name for that loop only. Shape and semantic validation bound size and count; the resolver composes the loop's skill set from carried and installed entries in spec order; the skill tool and system-prompt listing treat both alike; `loop_server.spec_policy.carried_skills` lets an operator refuse them and `GetCapabilities` advertises the setting; docs, example spec and SDK shape parity updated.

## Out of scope

Bundled skill files, slash commands or frontmatter beyond name and description; any tool grant attached to a skill; server-aware `ValidateHarness` (0104); a structured-output tool for the root loop.

## Reconcile log

### 2026-11-03

Unchanged against main 7ea0da7. SkillRef still name-only; SpecPolicy fields 1-3 in use, so carried_skills_allowed = 4 holds. Change 117 (CancelLoop RPC) also edits loop.proto in parallel; rebase before PR.
