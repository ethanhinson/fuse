<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0013 — ASCII art startup banner — shell init & fuse help](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0013-startup-banner.md)**
<!-- docket:backlink:end -->

# Startup Banner Design — Change 0013

## Overview

A plain-text ASCII banner printed at two call sites: interactive shell startup and `fuse help`. Owns one canonical string and one print function; no ANSI, no dynamic data, no flags.

## Banner content (canonical)

```
    ______    __  __   _____   ______
   / ____/   / / / /  / ___/  / ____/
  / /_      / / / /   \__ \  / __/
 / __/     / /_/ /   ___/ / / /___
/_/        \____/   /____/ /_____/

  multi-model agent harness  •  v{{VERSION}}

  fuse run <agent>   start an agent session
  fuse mcps          list connected MCP servers
  fuse help          show all commands
```

`{{VERSION}}` is substituted at build time from the binary's embedded version string (same source used everywhere else version is printed). One blank line above the wordmark, one blank line below the commands block, nothing else.

## Implementation

### Package

`internal/banner/banner.go` — the canonical banner in one place, exposed two ways:

```go
// String returns the startup banner with version interpolated.
func String(version string) string

// Print writes the startup banner to w.
func Print(w io.Writer, version string)
```

The banner is a package-level constant (the wordmark + quickstart lines) with `version` interpolated. `Print` wraps `String`. No dependencies beyond `fmt` and `io`. (`String` exists because the interactive call site injects the banner into a TUI scrollback buffer, not a raw writer — see call site 1.)

### Call sites

> **Reconciled 2026-08-05** against `origin/main` `a38c893`. The two call-site details below supersede the pre-build draft; the banner content, package, and version source are unchanged.

1. **Shell init — into the TUI scrollback, not a raw stdout print.** The interactive shell is an alt-screen bubbletea TUI (`cmd/fuse/shell.go`, `tea.WithAltScreen()`), so a stdout print before `p.Run()` is erased by the alt-screen switch. `NewShellModel` (`internal/tui/shell_model.go`, ~lines 218-219) already writes a two-line welcome into scrollback via `appendLine`; replace/augment that with `banner.String(version.Version)` so the banner appears at the top of the session scrollback. No `isatty` guard is needed here — the TUI only runs interactively by construction.

2. **`fuse help` — a new subcommand.** No `help` subcommand exists today; `cmd/fuse/main.go` dispatches `models`/`shell`/`mcps`/`mcp-server` + the default task run, with only a one-line stderr usage string (`main.go:66`). Add a `help` case to the dispatch that prints `banner.Print(stdout, version.Version)` followed by the command list. Also prepend the banner to the existing no-args usage output.

### Version source

`internal/version.Version` (`internal/version/version.go`) — the single version constant already used across the binary. No new mechanism.

## What is NOT included

- ANSI color or styling.
- `--no-banner` / `FUSE_NO_BANNER` suppression flag (deferred; add if users ask).
- Dynamic data (MCP count, model name, active skills).
- Tests beyond a smoke-test that `Print` writes non-empty output to a buffer.
