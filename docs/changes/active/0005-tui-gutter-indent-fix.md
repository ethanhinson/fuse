---
id: 5
slug: tui-gutter-indent-fix
title: Fix file-read gutter indentation in TUI
status: in-progress
priority: medium
type: fix
created: 2026-08-04
updated: 2026-08-04
depends_on: [2]
related: [2]
discovered_from: [2]
adrs: []
spec:
trivial: true
auto_groomable: false
branch: feat/tui-gutter-indent-fix
claimed_at: 2026-08-04T07:24:12Z
pr:
blocked_by:
reconciled: false
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
<!-- docket:artifacts:end -->

## Why

File read results in the TUI are over-indented. The `└` prefix on the first result line and the 5-space prefix on continuation lines don't align, and the combined `  └ ` + gutter ` │ ` pushes file content far to the right of the tool-call bullet it sits under. Visually jarring when reading multi-line file outputs.

## What changes

Tighten `appendResultLines` in `internal/tui/shell_model.go`:

- Fix the continuation-line prefix (`"     "` = 5 spaces) to match the first-line prefix width (`"  └ "` = 4 chars before the gutter). Currently off by one.
- Reduce overall leading whitespace so the gutter `│` column sits closer to the left margin, consistent with how the bullet `●` is positioned above it.
- Consolidate the gutter render into a single `gutterStyle.Render(number + " │ ")` call rather than two separate renders, which may be contributing extra spacing through lipgloss padding.

Exact pixel-level alignment confirmed against a live `fuse shell` session before merge.

## Out of scope

- Changing gutter behavior for non-file-read tools.
- Changing the `●` bullet alignment.
