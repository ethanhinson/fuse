<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0015 — Hanging-indent wrapping for the shell transcript](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0015-tui-hanging-indent-wrap.md)**
<!-- docket:backlink:end -->

# 0015 — Hanging-indent wrapping for the shell transcript

## Problem

Transcript lines carry per-line decoration — the `  └ ` result prefix, the
line-number gutter `  └ 1 │ ` / `    2 │ ` for file-read tools, the `● name(args)`
call bullets — but wrapping is applied later, flat, over the entire joined
transcript in `refreshViewport` (`internal/tui/shell_model.go:1093-1098`):

```go
content = wrap.String(wordwrap.String(content, m.vp.Width), m.vp.Width)
```

Neither reflow pass knows about the prefixes, so any logical line wider than the
viewport continues at **column 0**. Wrapped continuations of gutter lines escape
the gutter and read as separate top-level content; long unbroken tokens
(markdown link targets, URLs) get hard-split mid-word at the left margin.
Observed live reading `.docket/docs/changes/learnings/README.md` through
`read_file` in the shell view.

Prior art: change 0005 aligned the gutter prefixes; change 0006 added glamour
markdown for assistant prose. This change fixes what happens when a decorated
line exceeds the viewport width.

## Constraints (must hold)

1. **No emitted visual row may exceed viewport width.** The comment at
   `shell_model.go:1094-1096` is load-bearing: terminal-side wrapping desyncs
   the viewport's bottom-anchor math. Every code path below must keep emitting
   fully pre-wrapped content.
2. **Wrapping happens at refresh time, not append time.** `refreshViewport`
   re-wraps on every refresh and resize; wrapping at append time with the
   then-current width would double-wrap after a resize.
3. Styled prefixes contain ANSI sequences; all width math must be
   printable-width aware (`muesli/reflow/ansi.PrintableRuneWidth`; reflow's
   `wrap`/`wordwrap` are already ANSI-aware).

## Design

### 1. Structure the transcript store

`m.lines []string` becomes `[]transcriptLine`:

```go
type transcriptLine struct {
    first string // styled prefix for the first visual row (may be "")
    cont  string // styled prefix for continuation rows; same printable width as first
    text  string // the content (sanitized, possibly styled)
    pre   bool   // pre-wrapped upstream (glamour); skip wordwrap, hard-wrap safety only
}
```

Prefix and content are stored separately (rather than a single pre-concatenated
string plus a hang width) so continuation rows can carry a *different* prefix
glyph than the first row — the chosen continuation style repeats the gutter
rule with a blank number:

```
● read_file({"path": "README.md"})
  └ 1 │ One curated finding per file; this
      │ index is the hint surface. Load it,
      │ then read only the files you need.
    2 │
    3 │ ## concurrency
```

Producers set the pair by construction (equal printable widths):

| Producer | first | cont |
|---|---|---|
| gutter result row 1 | `  └ N │ ` | `    ` + pad(gutterW) + `│ ` |
| gutter result row i>1 | `    N │ ` | same as above |
| plain result row 1 | `  └ ` | `    ` |
| plain result row i>1 | `    ` | `    ` |
| error result | `  ✗ ` | `    ` |
| tool call bullet | `● ` (+styled name) | `  ` |
| assistant (glamour) | `""`, `pre: true` | `""` |
| everything else (headers, prompt echo, footers) | `""` | `""` |

`appendLine` keeps its signature for plain lines (wraps each split line into a
zero-prefix `transcriptLine`); `appendResultLines` and the `ToolCallMsg`/
`ToolResultMsg` handlers populate the prefixed forms.

### 2. Indent-aware wrap in refreshViewport

Replace the single flat wrap with a per-line helper:

```go
func hangWrap(l transcriptLine, width int) []string
```

- Compute `contentW = width - ansi.PrintableRuneWidth(l.first)`, clamped to a
  minimum (8, matching `buildEventViewLines`) so pathological widths degrade to
  narrow-but-correct rather than panicking or emitting empty rows.
- `pre == false`: `wrap.String(wordwrap.String(l.text, contentW), contentW)`,
  then emit `l.first + rows[0]` and `l.cont + rows[i]` for the rest.
- `pre == true`: skip wordwrap (glamour already wrapped at render width) but
  still apply `wrap.String(l.text, width)` as a hard-wrap safety net —
  a no-op at the width glamour rendered for, and after a shrink resize it
  preserves constraint 1 instead of overflowing the viewport.
- The transient rows `refreshViewport` composes itself (spinner + pending-call
  line, completer overlay) keep the current flat wrap.

### 3. Glamour output stops being re-wrapped

Assistant messages (`AssistantMsg` handler, `shell_model.go:360-368`) append
with `pre: true`. Today glamour's output — wrapped at `msg.Width` with its own
margins and indented blocks — goes through `wordwrap` a second time, which can
fold code blocks and blockquote indents to column 0; with `pre` it flows
through untouched except the hard-wrap safety net.

## Out of scope (deferred, noted during design)

- Re-rendering assistant markdown from source on resize (would need the raw
  markdown stored per message; today's behavior — old messages stay wrapped at
  their original width — is unchanged).
- The `… (+N more lines — /verbose…)` truncation footer is still numbered as if
  it were file content (`previewResult` output flows through the gutter path).
- The agents drilldown event view (`agents_model.go:492`) keeps its own flat
  wrap; it has no gutter, so continuations there are merely un-indented.

## Tests

Extend `shell_model_test.go`:

1. A gutter line wider than the viewport wraps with `    …│ ` continuation
   prefixes; the content column stays aligned; nothing lands at column 0.
2. A long unbroken token (URL/link target) hard-wraps *inside* the gutter.
3. No emitted visual row exceeds viewport width — including a `pre` line after
   the viewport shrinks (hard-wrap safety net).
4. Resize re-wraps from stored lines: append at width W, resize to W', assert
   continuations re-flow at W' (no double-wrap artifacts).
5. Styled prefixes: ANSI escapes in `first`/`cont` don't count toward width.
6. Plain (`└`) results and error (`✗`) results indent continuations 4 spaces.
