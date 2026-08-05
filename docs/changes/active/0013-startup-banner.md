---
id: 13
slug: startup-banner
title: ASCII art startup banner — shell init & fuse help
status: in-progress
priority: medium
type: feat
created: 2026-08-04
updated: 2026-08-05
depends_on: []
related: [12]
discovered_from: []
adrs: []
spec: docs/superpowers/specs/2026-08-04-startup-banner-design.md
plan:
results:
trivial: false
auto_groomable: false
branch: feat/startup-banner
claimed_at: 2026-08-05T07:38:48Z
pr:
blocked_by:
reconciled: false
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-08-04-startup-banner-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-08-04-startup-banner-design.md) |
<!-- docket:artifacts:end -->

## Why

Fuse has no identity at the terminal. Every time the shell starts you get a blank prompt with no sense of what commands are available or where to begin. Claude Code solves this with a compact banner on startup — fuse needs the same. A well-placed ASCII wordmark + three-line quickstart turns the first ten seconds from confusion into orientation, at zero runtime cost.

## What changes

- A plain-text ASCII banner (Option 2 — classic slant style) that prints on shell init and when running `fuse help`.
- Banner content: the FUSE slant wordmark, a one-line tagline ("multi-model agent harness"), the binary version, and three getting-started command examples (`fuse run <agent>`, `fuse mcps`, `fuse help`).
- No ANSI color — plain ASCII only, maximum terminal compatibility.
- Rendered in two places: (1) at interactive shell startup, mirroring Claude Code's banner behavior, and (2) as the header of `fuse help` output.
- A `banner.go` (or equivalent) package that owns the string and the print function so both call sites share one source.

## Out of scope

- Color/theming support.
- Dynamic content in the banner (e.g. live MCP server count, active model name).
- A `--no-banner` flag (can be added later if users request it).

## Open questions

None — design settled in brainstorm session.
