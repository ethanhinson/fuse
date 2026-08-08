---
name: teatest-final-frame-via-finalmodel-view
slug: teatest-final-frame-via-finalmodel-view
title: Capture a TUI screenshot from FinalModel().View(), not teatest FinalOutput — and force a lipgloss color profile
hook: "teatest's FinalOutput returns the terminal teardown frame after a quit (nearly blank), so a TUI screenshot must render the final model's View() instead; and in a non-TTY test the default lipgloss profile is Ascii (no color), so force termenv.TrueColor around the View() call or the capture is monochrome"
promotion_state: candidate
changes: [18]
created: 2026-08-08
updated: 2026-08-08
topics: [tui, testing, teatest, bubbletea, lipgloss, screenshots]
---

When adding visual-confirmation captures to a bubbletea TUI test driven by
`teatest`, two non-obvious traps make the naive approach produce a blank,
colorless image:

1. **`tm.FinalOutput(t)` is the wrong source.** After the program quits (e.g. a
   Ctrl+C keypress), the last thing written to the output stream is the terminal
   *teardown* frame — cursor reset, cleared region — which is nearly empty
   (observed: a 43-byte capture of the whole transcript). Read the **final
   model's `View()`** instead: it re-renders the full settled viewport exactly as
   the user last saw it.

   ```go
   fm := tm.FinalModel(t, teatest.WithFinalTimeout(5*time.Second))
   sm := fm.(ShellModel)           // your root model
   raw := []byte(sm.View())
   ```

2. **lipgloss renders Ascii (no color) in a non-TTY test.** The styles resolve
   against the *default renderer's* color profile, which is `Ascii` when stdout
   isn't a terminal — so `View()` emits plain text and any freeze/PNG render is
   monochrome. Force a profile around the capture and restore it:

   ```go
   prev := lipgloss.ColorProfile()
   lipgloss.SetColorProfile(termenv.TrueColor)
   raw := []byte(sm.View())
   lipgloss.SetColorProfile(prev)
   ```

Render the captured ANSI to a PNG with `charmbracelet/freeze` (`freeze in.ansi -o
out.png --window`) as a **best-effort** step — LookPath it and skip silently if
absent, so the suite stays hermetic and freeze stays optional. Write artifacts to
an env-gated dir (`FUSE_SCREENSHOT_DIR`) else `t.TempDir()`, so a normal run
dirties no tree and a human/CI opts in to collect them.

## War story

(#18, PR #24) — the TUI screenshot harness (`internal/tui/harness_test.go`
`captureFrame`). The first cut read `FinalOutput` and produced a 43-byte blank
frame; switching to `FinalModel().View()` gave the full transcript but monochrome
(`.ansi` == `.txt`, freeze skipped). Forcing `termenv.TrueColor` around `View()`
produced the real colored frame (a ~1 MB PNG). Verified end-to-end against the MCP
e2e mock server showing text/image/resource/error content blocks all rendered.
