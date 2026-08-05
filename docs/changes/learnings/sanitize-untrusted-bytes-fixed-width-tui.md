---
name: sanitize-untrusted-bytes-fixed-width-tui
slug: sanitize-untrusted-bytes-fixed-width-tui
title: Sanitize every model- or tool-controlled string before a fixed-width terminal render
hook: "Any bytes the model or a tool produced can shear a fixed-width TUI — strip ESC/C0/C1/CR, expand tabs (compositor counts \\t as one cell, terminal expands it), NUL-sniff binaries before display, and hard-wrap so no line exceeds pane width"
promotion_state: retained
changes: [12]
created: 2026-08-05
updated: 2026-08-05
topics: [tui, rendering, sanitization, bubbletea, terminal]
---

A fixed-width compositor (bubbletea et al.) assumes it knows each cell's width. Bytes it didn't author break that assumption and desync row diffing — panes shear, content scribbles across the layout, sometimes the terminal itself corrupts. Sanitize at **every** fixed-width render site, not once at ingestion (new render paths keep appearing):

- **Strip ESC / C0 / C1 control bytes and `\r`** — raw escape sequences reprogram the terminal.
- **Expand tabs** — the compositor counts `\t` as one cell while the terminal expands it to the next tab stop; tab-indented source code shears panes and desyncs diffing.
- **Refuse binaries before display** — NUL-sniff file reads; Mach-O/ELF bytes in the transcript corrupt the terminal session.
- **Hard-wrap (wordwrap + wrap chain)** so no line can exceed pane width — width overflow is the same shear by another route.
- **Rune-safe truncation** everywhere a string is chopped to fit.

The producers to distrust are anything outside the TUI's own code: tool results, file contents, and model-controlled labels (node names, tool args) that get interpolated into tree views and status lines.

## War story

(#12, PR #10) — Fuse subagent TUI, second hardening round: the model read the compiled `fuse` binary and Mach-O bytes corrupted the terminal (fix: NUL sniff in `read_file`); tab-indented Go source sheared both panes and desynced bubbletea's row diffing (fix: `sanitizeDisplay` expands tabs, strips ESC/C0/C1/`\r`); event view and shell viewport gained hard-wrap chains; model-controlled labels are now sanitized at every fixed-width render site.
