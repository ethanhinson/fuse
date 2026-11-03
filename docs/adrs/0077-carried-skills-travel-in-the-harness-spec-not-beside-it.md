---
id: 77
slug: 'carried-skills-travel-in-the-harness-spec-not-beside-it'
title: 'Carried skills travel in the harness spec, not beside it'
status: 'Accepted'
date: '2026-11-03'
supersedes: []
reverses: []
relates_to: []
change: 118
---

## Context

Callers that compose harnesses for many principals need to supply skills per loop, but skills could only come from operator-installed skill paths. Loop-scoped secrets (change 0102) already travel beside the spec on StartLoop and are deliberately never recorded; skills have the opposite need: they shape behavior and must be reproducible and auditable.

## Decision

A SkillRef with a body is a "carried skill": name, description, and body only; no bundled files, no slash command, no tool grants. Carried skills are part of the harness spec, so they are validated offline, included in harness.Digest, recorded in loop.harness, and pinnable by digest token, unlike loop-scoped secrets (change 0102), which must never be recorded. A carried skill shadows an installed skill of the same name for that loop only.

Bounds: body <= 32768 bytes; description <= 1024 chars on one line, required when a body is present; <= 16 carried entries; <= 64 total skill entries; unique names; and <= 256 KiB of carried bodies in total (a semantic rule).

The operator floor is a policy switch, loop_server.spec_policy.carried_skills (default true). When false, carried entries are refused with fuse.skills.carried_refused. The setting is advertised as GetCapabilities SpecPolicy.carried_skills_allowed. It is a switch, not a mount of operator-provided skill content.

## Consequences

Callers can supply skills per loop without operator involvement, and the run stays reproducible: the digest, recorded harness, and digest pins cover skill content. Specs grow by up to 256 KiB of carried bodies, and skill content becomes visible wherever the harness is recorded, so callers must not put secrets in skill bodies. Carried skills are deliberately less capable than installed ones (no bundles, slash commands, or tool grants). Operators retain a single on/off floor.

## Alternatives considered

- A side-channel field on StartLoop like secrets: rejected because it would not be digested, recorded, or pinnable, losing reproducibility.
- Operator-only skill_paths: rejected because callers composing harnesses for many principals cannot supply skills.
- Allowing full skill frontmatter or bundled files: rejected as a larger surface and out of scope.
