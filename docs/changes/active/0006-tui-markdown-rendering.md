---
id: 6
slug: tui-markdown-rendering
title: Terminal Markdown Rendering
status: implemented
priority: medium
type: feat
created: 2026-08-04
updated: 2026-08-04
depends_on: [2]
related: [2, 5]
discovered_from: []
adrs: []
spec: docs/superpowers/specs/0006-tui-markdown-rendering.md
trivial: false
auto_groomable: false
branch: feat/tui-markdown-rendering
claimed_at: 2026-08-04T07:27:33Z
pr: https://github.com/ethanhinson/fuse/pull/7
blocked_by:
reconciled: true
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [0006-tui-markdown-rendering.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/0006-tui-markdown-rendering.md) |
<!-- docket:artifacts:end -->

## Why

Model responses are markdown but the TUI renders them as raw text — `**bold**` shows as literal asterisks, code fences appear as plain text. The agent loop delivers the full response at once (not streaming), so glamour can render it cleanly before it hits the viewport.

## What changes

- Add `charmbracelet/glamour` dependency (same Charm vendor as the rest of the TUI stack).
- Store a `*glamour.TermRenderer` on `ShellModel`, created with `WithAutoStyle()` + `WithWordWrap(vp.Width)` and recreated on every `WindowSizeMsg`.
- Apply it in the `AssistantMsg` handler only — tool results, errors, and headers are unaffected.
- Fallback to raw text if the renderer is nil or `Render` errors.

## Out of scope

- Per-chunk streaming rendering.
- Custom style overrides.
- Syntax highlighting for tool result output.
