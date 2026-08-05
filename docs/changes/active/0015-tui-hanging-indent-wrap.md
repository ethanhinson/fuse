---
id: 15
slug: tui-hanging-indent-wrap
title: Hanging-indent wrapping for the shell transcript
status: proposed
priority: medium
type: fix
created: 2026-08-05
updated: 2026-08-05
depends_on: []
related: [5, 6]
discovered_from: []
adrs: []
spec: docs/superpowers/specs/0015-tui-hanging-indent-wrap.md
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
| Spec | [0015-tui-hanging-indent-wrap.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/0015-tui-hanging-indent-wrap.md) |
<!-- docket:artifacts:end -->

## Why

Long lines in tool results overflow the transcript layout: wrapping is applied
flat over the whole joined transcript in `refreshViewport`, with no knowledge
of the per-line decoration (`└` result prefix, line-number gutter, `●` call
bullets). Continuation rows of a wrapped gutter line start at column 0,
visually escaping the gutter and reading as separate top-level content; long
unbroken tokens split mid-word at the left margin. Observed live reading a
markdown file through `read_file` in the shell view — prose-heavy output makes
it constant. Follow-up to 0005 (gutter alignment) and 0006 (glamour markdown).

## What changes

- Transcript lines carry their prefix structurally: `m.lines` becomes a slice
  of `transcriptLine{first, cont, text, pre}` instead of pre-concatenated
  strings.
- `refreshViewport` wraps each line individually with a hanging indent;
  continuation rows of gutter lines repeat the gutter rule with a blank number
  (`      │ `), keeping the vertical rule unbroken.
- Glamour-rendered assistant markdown is marked pre-wrapped: no second
  wordwrap pass (which can fold code blocks/indents to column 0), only a
  hard-wrap safety net so the viewport's bottom-anchor invariant holds after
  a shrink resize.
- Wrapping stays at refresh time, so resize re-flows correctly.

## Out of scope

- Re-rendering assistant markdown from raw source on resize.
- The truncation footer (`… (+N more lines…)`) receiving a gutter line number.
- The agents drilldown event view's flat wrap (`agents_model.go`) — no gutter
  there, consistent enough.

## Reconcile log

<!-- Appended by docket-implement-next's reconcile pass: dated entries of what changed. -->
