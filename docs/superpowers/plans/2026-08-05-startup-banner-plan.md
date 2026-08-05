<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0013 — ASCII art startup banner — shell init & fuse help](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0013-startup-banner.md)**
<!-- docket:backlink:end -->

# Plan — ASCII art startup banner (change 0013)

Implements docket change 0013. Spec: `docs/superpowers/specs/2026-08-04-startup-banner-design.md` (on `docket`).

> Authored inline (plan role degraded to `auto`: the resolved skill `superpowers:writing-plans` was not invocable — Unknown skill).

## Goal

One canonical plain-ASCII banner, printed at two orientation points:
1. Interactive shell startup — injected into the alt-screen TUI scrollback.
2. `fuse help` — a new subcommand — with the command list; also prepended to the no-args usage.

Version comes from `internal/version.Version`. No ANSI, no dynamic content, no `--no-banner` flag.

## Canonical banner content

```
    ______    __  __   _____   ______
   / ____/   / / / /  / ___/  / ____/
  / /_      / / / /   \__ \  / __/
 / __/     / /_/ /   ___/ / / /___
/_/        \____/   /____/ /_____/

  multi-model agent harness  •  v<VERSION>

  fuse run <agent>   start an agent session
  fuse mcps          list connected MCP servers
  fuse help          show all commands
```

One blank line above the wordmark is NOT emitted by the constant (the constant starts at the wordmark); the trailing structure is: wordmark, blank, tagline+version, blank, three command lines. `<VERSION>` is interpolated from the passed string. All lines are spaces + slashes + letters — no tabs, no ESC — so they pass cleanly through the TUI's `sanitizeDisplay` (learning: sanitize-untrusted-bytes-fixed-width-tui). Longest line stays well under 80 cols.

## Task 1 — `internal/banner` package (TDD)

**Test first** — `internal/banner/banner_test.go`:
- `TestString_ContainsVersion`: `String("9.9.9")` output contains `"9.9.9"` and the wordmark substring `"\\____/"` (a slice of the wordmark), and the three command names `"fuse run"`, `"fuse mcps"`, `"fuse help"`, and the tagline `"multi-model agent harness"`.
- `TestPrint_WritesSameAsString`: `Print(&buf, "1.2.3")` writes exactly `String("1.2.3")` and buf is non-empty.
- `TestString_NoTabsNoEsc`: output contains no `\t` and no `\x1b` byte (guards the fixed-width-TUI render site).

**Implement** — `internal/banner/banner.go`:
- Package comment.
- Package-level `const wordmark = ...` (raw string literal of the wordmark + template lines with a `%s` or `{{VERSION}}` placeholder for the version) — choose `fmt.Sprintf` interpolation.
- `func String(version string) string` — returns the interpolated banner (ends with a single trailing newline).
- `func Print(w io.Writer, version string) { fmt.Fprint(w, String(version)) }`.
- Imports: `fmt`, `io` only.

Verify: `go test ./internal/banner/...` green.

## Task 2 — `fuse help` subcommand + no-args usage (TDD)

**Test first** — extend `cmd/fuse/main_test.go`:
- `TestRun_Help`: `run([]string{"help"}, &out, &err)` returns 0, `out` contains the wordmark slice, the version `version.Version`, and lists the subcommands (`models`, `shell`, `mcps`, `help`).
- `TestRun_NoArgs_ShowsBanner`: `run(nil, &out, &err)` returns 2 (usage) and `err` (stderr) OR `out` contains the banner wordmark. (Match the existing convention: current no-args usage goes to stderr and returns 2 — prepend the banner to that stderr output, keep exit 2.)

**Implement** — `cmd/fuse/main.go`:
- Add `case "help":` to the subcommand switch (after `mcp-server`): `banner.Print(stdout, version.Version)`, then print a command list (one line per subcommand with a short blurb), return 0.
- In the no-args branch (currently `fmt.Fprintln(stderr, "usage: ...")` + `return 2`), prepend `banner.Print(stderr, version.Version)` before the usage line; keep `return 2`.
- Import `internal/banner` and `internal/version`.

Verify: `go test ./cmd/fuse/...` green.

## Task 3 — shell-init banner in TUI scrollback (TDD where feasible)

**Test** — the welcome lines are emitted in `NewShellModel` (`internal/tui/shell_model.go`). If `shell_model` has a testable seam for initial scrollback content, assert it now contains the banner (version + wordmark slice). If constructing a `ShellModel` in a test requires heavy dependencies (registry/build func), keep the change minimal and rely on the banner package's own tests plus a manual note; do NOT weaken or add flaky TUI tests. Prefer: if `NewShellModel` can be constructed with nil/stub deps enough to inspect `m.lines`/scrollback, add `TestNewShellModel_ShowsBanner`.

**Implement** — `internal/tui/shell_model.go`, `NewShellModel` (~lines 218-219):
- Replace the two `appendLine` welcome calls (`"Fuse  <alias>"` and the quickstart hint) with `m.appendLine(banner.String(version.Version))`. Keep any alias/model info that the old lines conveyed if it is load-bearing — fold the alias into a single follow-up line after the banner if needed (e.g. `m.appendLine(fmt.Sprintf("model: %s — /model NAME to switch, /verbose to toggle, /exit to quit", alias))`) so the user still sees the active alias and controls.
- Import `internal/banner` and `internal/version`.

Verify: `go build ./...` and `go test ./internal/tui/...` green.

## Task 4 — full-suite gate

- `go build ./...`
- `go test ./...` — must be green (finalize.gate = local).
- `go vet ./...` if part of the repo's standard checks (see Makefile).

## Out of scope (do not implement)

ANSI color/theming; `--no-banner`/`FUSE_NO_BANNER`; dynamic banner data (live MCP count, active model in the banner body itself); any tests beyond the smoke tests named above.
