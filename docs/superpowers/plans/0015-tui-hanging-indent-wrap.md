<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0015 — Hanging-indent wrapping for the shell transcript](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0015-tui-hanging-indent-wrap.md)**
<!-- docket:backlink:end -->

# Plan — 0015 Hanging-indent wrapping for the shell transcript

Spec: `docs/superpowers/specs/0015-tui-hanging-indent-wrap.md` (on the `docket` metadata branch).

> **Plan authored by docket-implement-next directly (auto-fallback).** The configured
> plan skill `superpowers:writing-plans` was not invocable in this environment
> (`Unknown skill`), so per the docket Skill-layer missing-skill rule the plan role
> degraded to `auto` and the running agent authored this plan. Method is the agent's
> choice; the artifact and stop-point (a plan file recorded in `plan:`) are unchanged.

## Goal

Make the shell transcript wrap decorated lines with a hanging indent so continuation
rows stay inside their gutter/prefix column instead of escaping to column 0, while
preserving the load-bearing invariant that **no emitted visual row exceeds viewport
width** (bottom-anchor math depends on it). All in `internal/tui/shell_model.go` +
`internal/tui/shell_model_test.go`.

## Approach & method

TDD throughout: each task writes a focused failing test first (or extends an existing
test), then the minimal code to pass, then commits. Build is executed task-by-task
(subagent-driven-development); one full-suite gate at the end; whole-branch review
before the PR. Wrapping stays at **refresh time** (`refreshViewport`), never append
time — resize must re-flow from stored structure.

Current relevant code (feature branch, cut from `origin/main` @ 451d5b5):
- `m.lines []string` — pre-concatenated styled rows (prefix already glued to text).
- `refreshViewport` (~L1076-1111): flat wrap `wrap.String(wordwrap.String(content, W), W)` at ~L1099.
- `appendLine` (~L926), `appendResultLines` (~L945, builds gutter/plain prefixes),
  `AssistantMsg` handler (~L362, glamour render), `ToolCallMsg`/`ToolResultMsg`
  handlers (~L372/L413, bullets), `previewResult` (~L433), `sanitizeDisplay` (already
  applied to tool output — keep it).
- Learnings finding `sanitize-untrusted-bytes-fixed-width-tui` (retained): the
  `wordwrap + wrap` hard-wrap chain must remain at every render site — width overflow
  is a compositor shear. This plan keeps that chain per-emitted-row.

## Tasks

### Task 1 — Introduce the `transcriptLine` struct and migrate the store (no behavior change)

- **Test first:** add a test asserting `m.lines` is a `[]transcriptLine` and that a
  round-trip through `appendLine("a\nb")` yields two zero-prefix entries whose rendered
  output (via a temporary flatten helper or `refreshViewport`) equals today's output at
  a wide viewport. Keep an equivalence/golden assertion so this task is provably
  behavior-preserving at width ≥ content.
- **Code:**
  - Define `type transcriptLine struct { first, cont, text string; pre bool }` (fields
    per spec §1).
  - Change `m.lines` from `[]string` to `[]transcriptLine`.
  - Update every producer that currently appends a pre-concatenated string:
    `appendLine` (zero-prefix, split on `\n` into one `transcriptLine` per row, `pre:false`);
    the `ToolCallMsg` spawn_agent inline path (L380-395) and bullet append (L429);
    `AgentDoneMsg` subagent footer/summary (L461-464); the inline-block index writes
    that mutate `m.lines[block.lineIdx]` (L819/832/834/875/917) — these now write/read
    `.text` (or the appropriate field) of a `transcriptLine`.
  - `refreshViewport` temporarily still flattens each `transcriptLine` to
    `first+text` and applies the existing flat wrap — this task changes the store
    only, not the wrap behavior.
- **Commit:** `refactor(0015): store transcript as []transcriptLine`.

### Task 2 — `hangWrap` helper (per-line indent-aware wrap)

- **Test first (spec Tests 1, 5, 6):**
  - A gutter line wider than viewport wraps; continuation rows carry the `    …│ `
    continuation prefix; content column aligned; nothing at column 0.
  - ANSI escapes in `first`/`cont` do not count toward width (`PrintableRuneWidth`).
  - Plain `└` and error `✗` results indent continuations 4 spaces.
- **Code:** add `func hangWrap(l transcriptLine, width int) []string` (spec §2):
  - `contentW = width - ansi.PrintableRuneWidth(l.first)`, clamped to min 8.
  - `pre == false`: `wrap.String(wordwrap.String(l.text, contentW), contentW)`,
    emit `l.first + rows[0]`, then `l.cont + rows[i]` for i>0.
  - `pre == true`: skip wordwrap, apply `wrap.String(l.text, width)` hard-wrap safety
    net only; emit rows as-is (first/cont are `""`).
  - Import `github.com/muesli/reflow/ansi` for `PrintableRuneWidth`.
- **Commit:** `feat(0015): add hangWrap indent-aware line wrapper`.

### Task 3 — Wire `hangWrap` into `refreshViewport`

- **Test first (spec Tests 1, 3, 4):**
  - Settled lines wrap via `hangWrap`; no emitted row exceeds `m.vp.Width` (assert over
    every output row, printable-width aware).
  - Resize re-wrap: append at width W, resize to W', assert continuations re-flow at W'
    (no double-wrap artifacts) — proves wrapping is at refresh time from stored structure.
  - The transient rows `refreshViewport` composes itself (spinner + pending-call line,
    completer overlay) keep the current flat wrap.
- **Code:** replace the flat join+wrap of settled content (L1080 join, L1099 wrap) with
  a loop that calls `hangWrap(l, m.vp.Width)` per stored `transcriptLine` and joins the
  emitted rows. Keep the spinner/pending-call and completer-overlay composition on their
  existing flat-wrap path (append after the wrapped settled block, as today). Preserve
  the top-pad and bottom-anchor logic unchanged.
- **Commit:** `feat(0015): wrap transcript per-line with hanging indent`.

### Task 4 — Populate gutter/plain/error prefixes in `appendResultLines`

- **Test first (spec Tests 1, 6):** a multi-line file-read result produces
  `transcriptLine`s whose `first`/`cont` match the spec's producer table — gutter row 1
  `first = "  └ N │ "`, `cont = "    "+pad(gutterW)+"│ "`; gutter row i>1 `first = "    N │ "`,
  same `cont`; plain result `first="  └ "`/`"    "`, `cont="    "`; error `first="  ✗ "`,
  `cont="    "`. Assert equal printable widths of `first` and `cont` for gutter rows so
  content columns align.
- **Code:** rewrite `appendResultLines` to build `transcriptLine{first, cont, text, pre:false}`
  per row instead of pre-concatenating (L954-972). `text` is the sanitized content only
  (prefix moved into `first`/`cont`). Keep `sanitizeDisplay`, `useGutter`, `gutterW`,
  `isFileReadTool` logic. Continuation prefix repeats the gutter rule with a blank number
  per spec §1 (`"    " + strings.Repeat(" ", gutterW) + "│ "`, styled to match).
- **Commit:** `feat(0015): structure result-line prefixes for hanging indent`.

### Task 5 — Mark glamour assistant output pre-wrapped

- **Test first (spec Test 3):** an assistant (glamour) `transcriptLine` has `pre:true`;
  after the viewport shrinks, no emitted row exceeds the new width (hard-wrap safety
  net fires) and code-block/indent structure is not folded to column 0 at the render
  width.
- **Code:** in the `AssistantMsg` handler (L362-370), append via a path that sets
  `pre:true` and `first/cont=""` (either a new `appendPre`-style helper or an
  `appendLine` variant). Glamour output flows through untouched except `hangWrap`'s
  `pre` safety net.
- **Commit:** `feat(0015): stop re-wrapping glamour output; hard-wrap safety net only`.

### Task 6 — Full-suite gate + polish

- Run `go test ./...` (and `go build ./...`) from the feature worktree; fix any
  fallout (inline-agent block index paths, event-view interplay).
- Confirm out-of-scope items are untouched: `agents_model.go` flat wrap, the
  truncation footer numbering, resize re-render of raw markdown.
- **Commit:** any final fixes as `fix(0015): …` / `test(0015): …`.

## Verification

- `go test ./internal/tui/...` green, including the six spec tests.
- `go build ./...` green.
- Manual (optional, per learnings `verify-from-feature-worktree-binary`): build the
  feature-worktree binary and read a long-line file through `read_file` in the shell
  view; confirm continuations stay in the gutter and no line overflows.

## Out of scope (from spec)

Re-rendering assistant markdown from source on resize; gutter-numbering the truncation
footer; the agents drilldown event view's flat wrap.
