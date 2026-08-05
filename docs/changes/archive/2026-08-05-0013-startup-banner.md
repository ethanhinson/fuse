---
id: 13
slug: startup-banner
title: ASCII art startup banner — shell init & fuse help
status: done
priority: medium
type: feat
created: 2026-08-04
updated: 2026-08-05
depends_on: []
related: [12]
discovered_from: []
adrs: []
spec: docs/superpowers/specs/2026-08-04-startup-banner-design.md
plan: docs/superpowers/plans/2026-08-05-startup-banner-plan.md
results: docs/results/2026-08-05-startup-banner-results.md
trivial: false
auto_groomable: false
branch: feat/startup-banner
claimed_at: 
pr: https://github.com/ethanhinson/fuse/pull/11
blocked_by:
reconciled: true
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-08-04-startup-banner-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-08-04-startup-banner-design.md) |
| Plan | [2026-08-05-startup-banner-plan.md](https://github.com/ethanhinson/fuse/blob/feat/startup-banner/docs/superpowers/plans/2026-08-05-startup-banner-plan.md) |
| Results | [2026-08-05-startup-banner-results.md](https://github.com/ethanhinson/fuse/blob/feat/startup-banner/docs/results/2026-08-05-startup-banner-results.md) |
| PR | [#11](https://github.com/ethanhinson/fuse/pull/11) |
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

## Reconcile log

### 2026-08-05 — implementer reconcile (pre-plan)

Reconciled the spec against current `origin/main` (`a38c893`). Design intent (a plain-ASCII orientation banner at two call sites) is sound and unchanged; two call-site details are adjusted to fit reality:

- **Version source confirmed** — `internal/version.Version` (`internal/version/version.go`) already exists and is the single version string; the banner uses it. No new mechanism, as the spec assumed.
- **Call site "shell init" — TUI scrollback, not a raw stdout print.** The spec assumed a plain REPL where `banner.Print(os.Stdout, ...)` before the prompt would be visible. The interactive shell is actually an **alt-screen bubbletea TUI** (`cmd/fuse/shell.go`, `tea.WithAltScreen()`); a stdout print before `p.Run()` is wiped by the alt-screen switch. `NewShellModel` (`internal/tui/shell_model.go:218-219`) already emits a two-line welcome (`"Fuse  <alias>"` + a quickstart hint) into scrollback via `appendLine` — that is the real orientation surface. The banner replaces/augments those lines, rendered into scrollback so it survives the alt-screen. The `banner` package therefore also exposes the banner as a string (not only a `Print(w, version)`), so the TUI can inject it via `appendLine`; the exact API is the plan's call.
- **Call site "`fuse help`" — the subcommand does not exist yet; creating it is in-scope.** `cmd/fuse/main.go` dispatches `models`/`shell`/`mcps`/`mcp-server` + the default task run; the only help-like output is a one-line stderr usage string (`main.go:66`). The spec names `fuse help` as a call site and lists it in the quickstart, so a `help` subcommand is added that prints the banner followed by the command list. The banner also prepends the existing no-args usage output.

No obsolescence, no fundamental invalidation — scope-adjusted only. No material adjacent follow-up work surfaced (auto-capture is disabled this repo regardless).
