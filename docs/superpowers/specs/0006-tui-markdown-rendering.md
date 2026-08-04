<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0006 — Terminal Markdown Rendering](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0006-tui-markdown-rendering.md)**
<!-- docket:backlink:end -->

# Terminal Markdown Rendering

**Change:** [0006-tui-markdown-rendering](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0006-tui-markdown-rendering.md)

## Context

Model responses arrive as raw markdown strings. The TUI currently appends them with a flat `assistantStyle.Render(text)` — a single lipgloss colour wrap that displays `**bold**` as literal asterisks and ` ```go ` as a plain fence. Every model that produces structured output (headers, lists, code blocks) looks worse than it should.

The agent loop calls `renderer.Assistant(resp.Content)` with the full response at once (not streaming chunks), so there is no buffering complexity — markdown can be rendered before the text is appended to the transcript.

## Library

**`github.com/charmbracelet/glamour`** — by Charm, same vendor as bubbletea/lipgloss/bubbles. Uses goldmark (CommonMark parser) + lipgloss styles internally; ships a built-in `auto` style (adapts to dark/light terminal background detection via termenv); supports headers, bold/italic, code blocks (chroma syntax highlighting), blockquotes, and lists. Used by `gh` for exactly this purpose.

No custom package. One `go get`.

## Architecture

### Stored renderer on `ShellModel`

```go
type ShellModel struct {
    // existing fields …
    md *glamour.TermRenderer  // nil until first WindowSizeMsg
}
```

The renderer is created (or recreated on resize) in `WindowSizeMsg` handling:

```go
case tea.WindowSizeMsg:
    // existing sizing …
    r, err := glamour.NewTermRenderer(
        glamour.WithAutoStyle(),
        glamour.WithWordWrap(msg.Width),
    )
    if err == nil {
        m.md = r
    }
    // …
```

`glamour.WithAutoStyle()` reads the terminal's background colour via termenv and selects the appropriate built-in palette. `WithWordWrap(msg.Width)` matches the viewport width so glamour's wrapping and bubbletea's viewport word-wrap cooperate rather than fight.

### Rendering site — `AssistantMsg` handler only

```go
case AssistantMsg:
    text := msg.Text
    if m.md != nil {
        if rendered, err := m.md.Render(text); err == nil {
            text = strings.TrimRight(rendered, "\n")
        }
        // on error: fall through with raw text — never blank the response
    }
    m.appendLine(assistantStyle.Render(text))
    return m, waitForMsg(m.ch)
```

`assistantStyle` currently only sets colour; after this change it may be a no-op wrapper or removed in favour of glamour's own palette — implementation decides.

**Applies to:** assistant prose only.  
**Does not apply to:** `ToolResultMsg`, `AgentErrMsg`, the model header line, the skill/command echo — those are already rendered by their own styles and do not contain user-facing markdown.

### Fallback

If `m.md` is nil (no `WindowSizeMsg` received yet — e.g. tests, piped output) or `Render` returns an error, the raw text is displayed. Never blank the response.

## Dependencies

```
github.com/charmbracelet/glamour  v0.x  (latest stable)
```

Glamour transitively pulls in chroma (syntax highlighting) and goldmark. Both are small, pure-Go, widely used.

## Out of scope

- Streaming / per-chunk incremental rendering (moot — full response arrives at once).
- Syntax highlighting for tool result output.
- Custom glamour style overrides beyond `auto`.
- Light-mode palette tuning (handled automatically by `WithAutoStyle()`).
