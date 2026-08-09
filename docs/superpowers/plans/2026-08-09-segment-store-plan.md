<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0030 — Segment store — pre-compaction transcript archive for replay](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0030-segment-store.md)**
<!-- docket:backlink:end -->

# Plan — Segment store (concrete filesystem SegmentSink) — change 0030

**Change:** #0030 · **Spec:** [../../../.docket/docs/superpowers/specs/2026-08-08-segment-store-design.md] (metadata branch)
**Feature branch:** `feat/segment-store` (cut from `origin/main`) · **Date:** 2026-08-09

> **Provenance note (Skill-layer degradation).** The resolved plan skill
> `superpowers:writing-plans` was **not invocable at runtime** ("Unknown skill"). Per the docket
> convention's Skill-layer *missing-skill rule*, this step **degraded to `auto`**: this plan file
> was authored by the implementer directly. The build role (`superpowers:subagent-driven-development`)
> is expected likewise to be checked at build time; if it too is unavailable the build degrades to
> the `auto` fallback (execute this plan on the feature branch with TDD) with the same prominent
> warning. This degradation is also surfaced in the PR body.

## Context (reconciled)

#0027 (anchored summarization) is **merged** and already ships the widened struct-based seam this
change was going to add. Ground truth confirmed against `origin/main` (bd6f140):

- `internal/agent/segment.go` — `SegmentRegion{TurnStart,TurnEnd,Messages,Summary,ToolNames,
  TokensBefore,TokensAfter}`, `SegmentSink.Archive(r SegmentRegion) (pointer string, err error)`,
  and `noopSegmentSink` returning `("", nil)`. **No seam work needed.**
- `internal/agent/loop.go:379` invokes `a.segmentSink.Archive(SegmentRegion{...})` **without**
  `TurnStart/TurnEnd` (left zero) — DRIFT 1 to fix here.
- `internal/agent/loop.go:190` `summarizationRegion(messages, protectTokens)` returns the region
  messages + `insertAt` + toolNames + tokens; the turn span must be derived from the region's
  original indices via `turnIndices` (`internal/agent/relevance.go`).
- `internal/agent/summarize.go` `buildSummaryMessage(summary, pointer)` emits `Recovery: grep your
  past at <path>` only when `pointer != ""`. **Already correct.**
- `internal/agent/agent.go:140` `SetSummarizer(s, sink)` / `:153` `EnableSummarization(c, modelID,
  maxOutput, sink)` — the sink injection points. `cmd/fuse/run.go:286` and
  `cmd/fuse/context_summarization_gateway_test.go:89` call with `nil` (→ noop). The **real sink is
  wired at `cmd/fuse/run.go:286`**.
- `cmd/fuse/shell.go`: `session.NewLogger(logDir)` (line 119) opens BEFORE `tree.RootID()`
  (line 156) exists — DRIFT 2. But `tree.RootID()` IS available before the `build` closure /
  `EnableSummarization`, so the **sink** can be keyed by root id even though the **log path** move
  needs care.
- `internal/session/log.go`: `Logger`, `NewLogger(dir)`, `DefaultLogDir()`, `SweepOld(dir, maxAge)`
  (non-fatal, hardcoded `*.jsonl`); called `go session.SweepOld(logDir, 7*24*time.Hour)`
  (`cmd/fuse/shell.go:118`).
- `internal/tools/registry.go`: `Tool` interface, `DefaultTools()`, `Registry.Subset`. Child wiring
  in `cmd/fuse/run.go` (`childToolRegistry`) and `cmd/fuse/main.go` (spawn/subset).
- `internal/tui/agents_model.go`: `renderDetailHeader`, `handleDetailKey` (keys j/k/g/G/enter/q/esc/
  tab — `s` is FREE), `buildEventViewLines` (drill-in pane, already sanitizes).
- `internal/model/message.go`: `Message{Role, Content, ToolCalls, ToolCallID, Name}`.

## Scope

Build the concrete filesystem sink + index, populate the turn range, wire the real sink, add the
`segment_read` tool, parameterize + add segment GC, and add the TUI surface. **Dropped from the
original spec (already done by #0027):** widening the seam (D1) and amending #0027's spec (item 7).

## Out of scope (restate in PR)

`fuse replay` command; segment encryption; segment compression; cross-session `segment_read`;
migrating existing flat `*.jsonl` logs into the per-session dir (old logs stay flat, read-compatible).

## Design decisions locked for the build

- **Session id = root `AgentNode.ID`** (`tree.RootID()`), consumed where it is available (agent
  construction / `EnableSummarization`), NOT at `NewLogger` time.
- **Per-session directory** `~/.fuse/sessions/<session-id>/` containing `session.jsonl` and
  `segments/`. To sidestep DRIFT 2 with the least churn: the **sink** owns the per-session dir and
  creates it lazily on first `Archive` (`os.MkdirAll` of `<sessions>/<id>/segments`). The
  **session-log path move** (log under `<id>/session.jsonl`) is done by threading the root id into
  logger construction *after* the tree exists — i.e. move `NewLogger` to after `tree.RootID()` in
  `cmd/fuse/shell.go`, or add `NewSessionLogger(baseDir, sessionID)` and open it post-tree. The
  plan prefers **relocating the `NewLogger` call to after tree construction** (smallest surface;
  the logger is only referenced by the `build` closure which runs later). Flat logs remain
  readable; no migration.
- **Segment file** `segments/<turnStart>-<turnEnd>-<seq>.md` with YAML front-matter
  (`turn_start,turn_end,tools,tokens_before,tokens_after,ts`) + `## Summary` + `## Raw region`.
- **`index.json`** one per session: `{session_id, segments:[{turn_start,turn_end,tools,
  tokens_before,tokens_after,path,ts}]}`. `Archive` = write one file + append one entry
  (read-modify-write; single-writer within a session — the loop is single-goroutine per agent, and
  the root session sink is only driven by the root loop, so no cross-process lock needed; guard the
  RMW with an in-process mutex on the sink for safety).
- **Returned pointer** = absolute path of the written segment file (what `buildSummaryMessage`
  interpolates into the recovery line).

## Tasks (TDD; each independently committable)

### Task 1 — Turn range at the sink invocation (DRIFT 1)
- **Test first** (`internal/agent/loop_test.go` or `segment_test.go`): a summarization pass over a
  crafted message slice hands the sink a `SegmentRegion` whose `TurnStart/TurnEnd` equal the min/max
  turn indices of the compacted region (use `turnIndices`). Use a capturing fake sink.
- **Impl:** make `summarizationRegion` also return the region's first/last original indices (or the
  turn span directly), then in `loop.go` compute `ti := turnIndices(messages)` and set
  `TurnStart: ti[firstIdx], TurnEnd: ti[lastIdx]` on the struct at line ~379.
- Verify `go test ./internal/agent/...` green; `go build ./...`.

### Task 2 — Segment file + index schema types & serialization
- **Test first** (`internal/segment/` new pkg, e.g. `store_test.go`): round-trip the segment file
  format (front-matter + `## Summary` + `## Raw region`) and the `index.json` schema; assert exact
  field names and that `path` is relative to `segments/`.
- **Impl:** a small `segment` package (or under `internal/session/`) with the file-render + index
  read/append helpers. Render raw-region messages as `role`/`[tool name]`/content; do NOT sanitize
  here (that is a TUI render concern) — store raw.
- Green tests; build.

### Task 3 — Concrete `FSSegmentSink` implementing `agent.SegmentSink`
- **Test first:** `Archive(r)` writes exactly one `<turnStart>-<turnEnd>-<seq>.md` under the
  session `segments/` dir, appends exactly one `index.json` entry, returns the **absolute** file
  path; `<seq>` disambiguates a repeated turn range; lazy-creates the session dir on first call;
  concurrent `Archive` calls (mutex) don't corrupt `index.json`. Confirm the existing
  `noopSegmentSink` still returns `("", nil)` writing nothing (unchanged).
- **Impl:** `FSSegmentSink` holding the resolved `<sessions>/<session-id>/segments` dir + a mutex;
  implements `Archive`. Constructor takes base sessions dir + session id.
- Note the import boundary: the sink implements `agent.SegmentSink`. Place it where it can be
  constructed by `cmd/fuse` and satisfies the interface without an import cycle (likely
  `internal/segment` imported by `cmd/fuse`, passed into `agent` via the existing interface).
- Green; build.

### Task 4 — Wire the real sink + per-session log dir in `cmd/fuse`
- **Test first:** a `cmd/fuse` test (or extend the gateway test) asserting that a real run
  constructs the FS sink keyed by `tree.RootID()` and that `EnableSummarization` receives it (not
  nil). Assert the session log path is under `~/.fuse/sessions/<root-id>/session.jsonl` for a new
  session; an existing flat `*.jsonl` still loads.
- **Impl:** in `cmd/fuse/shell.go`, relocate `NewLogger` to after `tree.RootID()` and open under the
  per-session dir; construct `FSSegmentSink(baseSessionsDir, tree.RootID())`; pass it into
  `EnableSummarization` at `cmd/fuse/run.go:286` (thread the sink through the `build` closure /
  agent construction). Keep the `nil`→noop default for the one-shot/probe paths unless the spec's
  archival is wanted there too — decide per path; at minimum the interactive shell wires the real
  sink.
- Green; build.

### Task 5 — `segment_read(turn_range, tool_filter?)` built-in tool
- **Test first** (`internal/tools/segment_read_test.go`): single turn `"12"` and range `"12-18"`
  resolve **overlapping** segments from `index.json`; `tool_filter` narrows returned messages to
  matching `Name`; an oversized selection returns a bounded slice + trailer `"N more
  segments/messages match — narrow turn_range or set tool_filter"`; an empty range returns a clean
  "no segments" result (not an error); reads only its own session's `segments/`.
- **Impl:** new `Tool` in `internal/tools/` (`Name/Description/Parameters/Execute`); resolve the
  running session's segments dir (inject the session dir/path — the tool needs to know its session;
  pass it at construction from `cmd/fuse`, mirroring how spill dir is set). Output-bounded up front.
- **Add to `DefaultTools()`** in `internal/tools/registry.go`.
- Green; build.

### Task 6 — Child-registry wiring re-verification (learning: patch-every-cloned-child-builder)
- **At build time, GREP** for every child-registry construction site — `Registry.Subset`,
  `childToolRegistry`, `DefaultTools` callers, `spawn` wiring in `cmd/fuse/run.go` and
  `cmd/fuse/main.go` (and re-check for any 4th site). Confirm `segment_read` flows through each
  intended site (a child that omits it via an explicit `tools` subset is fine — verify it is
  present where NOT explicitly omitted). **Note:** `segment_read` reads the *session's own*
  segments — decide whether children get their own session dir or share the root's; default to
  sharing the root session (a child reads the same session's segments) unless spec implies
  otherwise, and document the choice.
- **Test:** an assertion that `DefaultTools()` includes `segment_read` and that a subset built
  without an explicit omit list contains it.
- Green; build.

### Task 7 — GC: parameterize `SweepOld` + 14-day segment sweep, symlink-safe
- **Test first** (`internal/session/log_test.go`): `SweepOld(dir, maxAge, pattern)` still sweeps
  `*.jsonl` at 7 days (existing behavior preserved via the new pattern arg); a new segment sweep
  removes a `>14d` `segments/*.md` and prunes its `index.json` entry, keeps a `<=14d` one, removes
  an emptied session dir; a **symlinked** session dir is descended (learning:
  dirent-isdir-skips-symlinks — fall back to `os.Stat` when `DirEntry.IsDir()` is false).
- **Impl:** add a `pattern` parameter to `SweepOld` (update the `cmd/fuse/shell.go:118` call to pass
  `"*.jsonl"`); add a segment sweep function over `~/.fuse/sessions/*/segments/*.md` at 14 days with
  index pruning; fire it alongside the log sweep at session start in `cmd/fuse/shell.go`.
- Green; build.

### Task 8 — TUI: indicator + `s` "show original"
- **Test first** (`internal/tui/agents_model_test.go`): `renderDetailHeader` renders a compact
  segments line (e.g. `◆ 3 segments · 12KB→1.2KB`) when the selected node has archived segments and
  omits it when none; an `s` keypress in `handleDetailKey` on a summarized node opens the raw region
  in the existing drill-in (`buildEventViewLines`) — reuse it (learning:
  sanitize-untrusted-bytes-fixed-width-tui — the drill-in already sanitizes; do NOT add a new render
  path).
- **Impl:** segment metadata source — prefer a compaction event the loop already emits if cheap;
  else a cached read of `index.json`. Add the `s` case to `handleDetailKey`.
- Green; build.

### Task 9 — Real-binary gateway-seam verification (learning: verify-tool-loop-at-gateway-seam)
- Drive the **shipped binary** against a scripted `LLM_GATEWAY_URL` double that forces a compaction.
  Assert: (a) a segment file + `index.json` entry are written under `~/.fuse/sessions/<id>/`;
  (b) the post-compaction request carries the summary with the `Recovery: grep your past at <path>`
  line; (c) a subsequent `segment_read` call round-trips the raw content. This exercises the
  `cmd/fuse` wiring the teatest/faked-Completer harness cannot reach. Model this on the existing
  `cmd/fuse/context_summarization_gateway_test.go`.
- Green under `-race` where feasible.

### Task 10 — Full-suite gate + lint
- `go build ./...`, `go test ./... -race` (or the repo's `make` target), `go vet`/lint. Fix any
  fallout. Confirm no metadata files were touched on the feature branch.

## Risks / notes carried into the build

- **`index.json` RMW** — guarded by an in-process mutex on the sink; single-writer per session
  assumed (verify no concurrent root archives). If children get their own sinks, each owns its own
  session dir → no shared-file contention.
- **Sink import boundary** — implement the concrete sink outside `internal/agent` (which only
  defines the interface) to avoid an import cycle; `cmd/fuse` constructs and injects it.
- **`segment_read` session scoping** — the tool must resolve *its* session's dir; thread the
  session dir at construction (like `SetSpillDir`).
- **TUI metadata chattiness** — reading `index.json` per header render is chatty; cache or ride a
  compaction event. Decide at Task 8.

## Scope expansion — compression & non-destructive GC (PR #41 follow-up)

Correction to the original design: the age sweeps deleted data and segments were
stored uncompressed. New tasks make archival gzip-compressed, non-destructive,
and self-describing, gated by an antagonistic answer-quality suite.

### Task 11 — `internal/archive` helper (TDD)
- `Archive(path, MetaFunc)`: gzip `path` → `path+".gz"` (0o600), write a
  `path+".gz.meta.yml"` YAML sidecar (common fields + caller domain fields),
  `os.Remove` the original. Idempotent on `.gz` / pre-existing `.gz`.
- `Open(path)`: transparent read — prefer `path`, fall back to `path+".gz"`,
  gunzip on magic bytes. Generic (no `session`/`tools` import).
- Tier-1 round-trip tests incl. one `.gz` from the SYSTEM `gzip` binary.

### Task 12 — Born-compressed segment seam (TDD)
- `FSSegmentSink.Archive` writes gzip → `<n>.md.gz`; index records the on-disk
  name; `index.json` stays uncompressed. `segment.LoadSegment` gunzips
  transparently + `.gz` path fallback. Keep the lossless JSON raw-region format.

### Task 13 — Convert the three sweeps delete → gzip-archive (TDD)
- `session.SweepOld` (logs): gzip + session-domain sidecar, skip `.gz`.
- `session.SweepOldSegments`: non-destructive back-compat bridge — compress
  legacy plaintext `.md` in place, re-point index Path, never delete/prune;
  born-compressed skipped. No second destructive horizon.
- `tools.sweepSpillDir`: gzip + spill-domain sidecar, skip `.gz`/`.meta.yml`.

### Task 14 — `read_file` gzip-transparent (TDD)
- `tools.read` opens via `archive.Open`; binary guard on decompressed bytes;
  spill recovery hint resolves post-archival.

### Task 15 — Antagonistic answer-quality suite (TDD, the gate)
- Tiers 1–5 including the Tier-4 gz-vs-plaintext end-to-end parity gate through
  the real `a.Run` loop (mirrors the bd6f140 relevance-rescue drive test).

### Task 16 — Full-suite gate under `-race`, `go vet`, docs, ADR-0020.
