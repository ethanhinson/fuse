---
id: 81
slug: shell-transcript-command-breaks
title: Echo executed slash commands + rule breaks between transcript blocks
status: proposed
priority: medium
type: feat
created: 2026-09-01
updated: 2026-09-01
depends_on: []
related: [10, 78, 79, 80]
discovered_from: [80]
adrs: []
spec:
plan:
results:
trivial: true
auto_groomable:
branch:
pr:
blocked_by:
reconciled: false
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
<!-- docket:artifacts:end -->

## Why

The `fuse shell` transcript gives no visual structure to command output. When a
slash command runs, `handleSlash` appends only its output — the executed command
itself is never echoed — and consecutive blocks stack with no separator. In
practice the `/models` listing runs straight into the next thing on screen (e.g.
the slash-command completer overlay), so it reads as one undifferentiated wall of
text with no cue for where one command's output began.

Task prompts already echo as `> <text>` when a run starts (`startPrompt`), which
is exactly the affordance slash commands lack. We want the same treatment for
slash commands, plus a break between blocks, so the transcript reads like Claude
Code's: each executed command visible above its output, each block delineated.

## What changes

- **Echo the executed slash command** into the transcript as a header above its
  output (the `> /models` form), mirroring how task prompts already echo — so a
  reader can see which command produced each block.
- **A faint rule line between transcript blocks**, using the existing
  `ruleStyle`, so consecutive command outputs are visually separated rather than
  abutting.

Applies to the builtin and skill/MCP slash-command paths that render output into
the transcript.

## Out of scope

- The shared table component and tabbed `/config` UI (change 0080) — that is the
  column-alignment work; this is the block-separation/echo work. They compose but
  are tracked separately.
- Restyling the completer overlay itself (it is a live overlay, not a committed
  transcript block).
- Any change to how task-prompt runs echo — that already works; this only brings
  slash commands up to the same bar.

## Open questions

- Whether the rule spans the full viewport width or is a short divider, and
  whether the echo carries the `[alias]` prefix or just `>` — settle against the
  existing theme at build time.
