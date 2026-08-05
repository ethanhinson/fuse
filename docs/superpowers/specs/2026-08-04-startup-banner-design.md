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

`internal/banner/banner.go` — one exported function:

```go
// Print writes the startup banner to w.
func Print(w io.Writer, version string)
```

The banner string is a package-level constant (the wordmark lines) with `version` interpolated via `fmt.Fprintf`. No dependencies beyond `fmt` and `io`.

### Call sites

1. **Shell init** — wherever the interactive REPL loop currently starts (e.g. `cmd/fuse/main.go` or the shell entrypoint), call `banner.Print(os.Stdout, version)` once, before the prompt appears. Guard with `isatty(os.Stdout.Fd())` so piped/scripted invocations stay clean.

2. **`fuse help`** — at the top of the help output, before the command list. Same call, same function.

### Version source

Use the same `version` string already threaded through the binary (via `-ldflags` or `debug.ReadBuildInfo`). No new mechanism needed.

## What is NOT included

- ANSI color or styling.
- `--no-banner` / `FUSE_NO_BANNER` suppression flag (deferred; add if users ask).
- Dynamic data (MCP count, model name, active skills).
- Tests beyond a smoke-test that `Print` writes non-empty output to a buffer.
