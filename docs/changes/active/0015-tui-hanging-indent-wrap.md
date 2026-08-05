---
id: 15
slug: tui-hanging-indent-wrap
title: Hanging-indent wrapping for the shell transcript
status: in-progress
priority: medium
type: fix
created: 2026-08-05
updated: 2026-08-05
depends_on: []
related: [5, 6]
discovered_from: []
adrs: []
spec: docs/superpowers/specs/0015-tui-hanging-indent-wrap.md
plan: docs/superpowers/plans/0015-tui-hanging-indent-wrap.md
results:
trivial: false
auto_groomable:
branch: feat/tui-hanging-indent-wrap
pr:
claimed_at: 2026-08-05T08:29:21Z
blocked_by:
reconciled: true
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [0015-tui-hanging-indent-wrap.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/0015-tui-hanging-indent-wrap.md) |
| Plan | [0015-tui-hanging-indent-wrap.md](https://github.com/ethanhinson/fuse/blob/feat/tui-hanging-indent-wrap/docs/superpowers/plans/0015-tui-hanging-indent-wrap.md) |
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

### 2026-08-05 — reconcile before build

Re-read the change body + spec against related changes 5 and 6 (both `done`),
the ADR ledger, and the current `internal/tui/shell_model.go`. Findings:

- **Design still valid, no scope change.** Every code anchor the spec cites is
  present and semantically unchanged: `m.lines` is still `[]string`
  (pre-concatenated), the flat wrap `content = wrap.String(wordwrap.String(
  content, m.vp.Width), m.vp.Width)` with its load-bearing bottom-anchor comment
  is now at `refreshViewport` around line 1099 (spec cited 1093-1099),
  `appendResultLines` at line 945, `previewResult` at line 433, the
  `AssistantMsg` handler in the same block the spec references.
- **Line-number drift only.** Anchors moved ~+6 lines from change 0013
  (startup-banner, merged as PR #11) — additive, untouching the wrap path. The
  spec's line citations are approximate; the code structure is intact.
- **Naming nit (cosmetic).** The model type is `ShellModel` (exported), not the
  spec's lowercase `shellModel`. Plan/build use the real name; no design impact.

No adjacent follow-up work surfaced (auto-capture is disabled this repo).
Scope, tests, and out-of-scope list carry forward unchanged.
