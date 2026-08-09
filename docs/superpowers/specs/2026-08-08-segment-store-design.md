<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0030 — Segment store — pre-compaction transcript archive for replay](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0030-segment-store.md)**
<!-- docket:backlink:end -->

# Segment store — pre-compaction transcript archive for replay

**Change:** [#0030](../../changes/active/0030-segment-store.md) · **Status:** design · **Date:** 2026-08-08

## Problem

Anchored summarization (#0027, `proposed`, specced but not yet built) compresses old
tool-result regions into a structured ODSNF summary to reclaim context space. Compression is
**lossy**: after a region is summarized and pruned, the raw text is gone and the model can only
recover a detail by trusting the summary or re-running the tool. #0027 ships a `SegmentSink`
seam with a **no-op default** precisely so a later change can persist the raw region without
#0027 owning a disk writer.

**This change (#0030) implements the real sink**: it persists each pre-summarization region to
disk, indexes it, exposes a `segment_read` recovery tool the model can call on demand, surfaces
segments in the TUI, and sweeps old segments on a GC window. It also lays the storage
foundation for a future `fuse replay` command (out of scope here — this change only stores the
data).

## Dependency & coordination

- `depends_on: [27]`. #0027 owns the summarize → prune → inject flow and the `SegmentSink`
  interface; #0030 owns the concrete filesystem sink, the index, the tool, the TUI surface, and
  GC. #0027 is not yet built, so this spec is designed against #0027's **spec**, and the
  implementer's reconcile pass must re-validate against merged reality at build time.
- **Interface widening (settled with the human).** #0027's D1 seam is
  `Archive(region []model.Message) (pointer string, err error)`. That forces #0030 to
  reverse-engineer turn numbers and savings from a bare message slice. Instead we **widen the
  seam** to pass explicit turn range + metadata (see *Design D1*). This spec **amends #0027's
  spec** (a note recording the widened `SegmentRegion` struct) so whoever builds #0027 ships the
  correct seam; #0030 references it. The no-op default and #0027's recovery-pointer emission
  rule are unchanged.

## Design decisions (settled)

The high-level shape was decided with the human up front; the sub-decisions were settled in an
inline design conversation. **`superpowers:brainstorming` was unavailable in the grooming
session**, so the design was reached inline with the human (docket Skill-layer missing-skill
fallback) and is recorded here as final.

### D1 — Widened `SegmentSink` seam (a struct, not positional args)

Replace #0027's positional signature with a struct so the sink receives the turn range and
savings metadata directly rather than inferring them:

```go
// internal/agent/ (co-located with #0027's summarize flow)
type SegmentRegion struct {
    TurnStart, TurnEnd int             // inclusive turn range being compacted
    Messages           []model.Message // raw pre-summary region (role=="tool" spans + their calls)
    Summary            string          // the ODSNF summary that replaced the region
    ToolNames          []string        // distinct tool names in the region (cheap index grep key)
    TokensBefore       int             // estimate of the raw region
    TokensAfter        int             // estimate of the replacing summary
}

type SegmentSink interface {
    // Archive persists the raw pre-summarization region and returns a recovery
    // pointer (path) to hand back to the summary, or "" if nothing was persisted.
    Archive(r SegmentRegion) (pointer string, err error)
}
```

- **No-op default** (#0027's) returns `("", nil)` — unchanged in spirit; #0027 still emits the
  "grep your past at `<path>`" recovery-pointer line **only** when a real sink returns a
  non-empty pointer. With the no-op sink the pointer line is omitted.
- `Archive` errors are **best-effort** — logged, never fatal to the turn (#0027's invariant).
- `TurnStart/TurnEnd` come from the loop's turn counter at compaction time; #0027 already knows
  the candidate span, so it can populate the struct without new bookkeeping.

### D2 — Per-session directory, keyed by the root `AgentNode.ID`

Sessions are flat today (`~/.fuse/sessions/YYYY-MM-DD-XXXXXX.jsonl`; `internal/session/log.go`:
`Logger` @26, `NewLogger` @38). This change introduces a **per-session directory**:

```
~/.fuse/sessions/<session-id>/
    session.jsonl            # the existing JSONL log, moved under the dir
    segments/
        <turnStart>-<turnEnd>-<seq>.md
        index.json
```

- **`<session-id>` = the root `AgentNode.ID`** (settled). No new id generator; segments tie
  directly to the agent tree the TUI already renders. The id must be known when the logger opens
  — verify the ordering at build (the logger may need the root node id threaded in at
  construction, or the dir created lazily on first write).
- **Back-compat:** existing flat `*.jsonl` logs are **left as-is** — read-compatible, and the
  log GC still sweeps them. New sessions use the dir layout. No migration of old logs.
- This is a real restructure of `internal/session/log.go` (session-id acceptance, dir creation,
  log path move); accepted deliberately per the human's decision, and matches the `fuse replay`
  vision.

### D3 — Segment file format & index schema (grep-cheap)

**Segment file** `segments/<turnStart>-<turnEnd>-<seq>.md`:

```markdown
---
turn_start: 12
turn_end: 18
tools: [read_file, grep, bash]
tokens_before: 12040
tokens_after: 1180
ts: 2026-08-08T17:22:04Z
---

## Summary

<the ODSNF summary that replaced this region>

## Raw region

<the full pre-summary transcript: each message rendered role + tool name + content>
```

**`index.json`** (one per session, flat array — cheap to grep for a tool name):

```json
{
  "session_id": "<root AgentNode.ID>",
  "segments": [
    {"turn_start": 12, "turn_end": 18, "tools": ["read_file","grep","bash"],
     "tokens_before": 12040, "tokens_after": 1180, "path": "12-18-1.md",
     "ts": "2026-08-08T17:22:04Z"}
  ]
}
```

`Archive` writes **one file per call** and appends **one entry** to `index.json` (read-modify-
write under the session; single-writer within a session so no cross-process locking needed —
verify at build). `path` is relative to `segments/`. The returned pointer is the absolute
segment file path (what #0027 puts in the recovery line). `<seq>` disambiguates the rare case
of two archives sharing a turn range.

### D4 — `segment_read(turn_range, tool_filter?)` built-in tool

New tool in `internal/tools/` implementing the `Tool` interface (`Name`/`Description`/
`Parameters`/`Execute`; `registry.go`:20). Added to `DefaultTools()` (`registry.go`:141) so it
**auto-wires** to every entry point (shell, one-shot `run()`, research-probe, mcp-server) via
`defaultToolRegistry()`. **Build-time discipline:** per the `patch-every-cloned-child-builder`
learning, re-derive the child-subset / `spawn_agent` re-registration sites by grep at build and
confirm `segment_read` flows through each (`Registry.Subset` clones by name; a tool absent from
a child's subset is simply unavailable — that is fine, but verify it is present where intended).

- **`turn_range`**: `"12"` (single) or `"12-18"` (inclusive range).
- **Resolution**: read `index.json`, select segments whose `[turn_start, turn_end]` **overlaps**
  the requested range, load their raw regions.
- **`tool_filter`** (optional): narrow the returned messages to those whose `Name` matches the
  filter (a tool name), so the model can pull just the `read_file` results from a range.
- **Returns**: the matched raw region(s) as markdown. **Output guard**: the registry's `Execute`
  already spill-truncates oversized output, but the tool itself bounds up front — if the
  selection exceeds a cap, return the first bounded slice plus a trailer
  (`"N more segments/messages match — narrow turn_range or set tool_filter"`) rather than
  dumping. Reads only its own session's `segments/` (path derived from the running session id;
  no cross-session access).

### D5 — GC: parameterized sweep, 14-day segment window, symlink-safe

Generalize the log sweeper and add a segment sweep:

- Parameterize `SweepOld(dir string, maxAge time.Duration, pattern string)` (today it globs
  `*.jsonl` flat with a hardcoded 7-day window at `shell.go`:117). The log call keeps
  `*.jsonl` / 7 days.
- Add a **segment sweep** with a **14-day** window over `~/.fuse/sessions/*/segments/*.md`
  (plus the session dir's `index.json` pruned/rewritten to drop swept entries; an emptied
  session dir may be removed). Fire it alongside the existing log sweep at session start.
- **Symlink safety** (`dirent-isdir-skips-symlinks` learning): when descending into session
  dirs, fall back to `os.Stat` rather than trusting `DirEntry.IsDir()`, so a symlinked session
  dir is not silently skipped.

### D6 — TUI: indicator + "show original" via the existing drill-in

`internal/tui/agents_model.go`:

- **Indicator**: in `renderDetailHeader()` (@601), when the selected node has archived segments,
  render a compact line, e.g. `◆ 3 segments · 12KB→1.2KB` (count + before→after size). Segment
  metadata is read from the session `index.json` (or surfaced via an event the loop already
  emits on compaction — verify which is cheaper at build).
- **"Show original" action**: add an `s` keypress in `handleDetailKey()` (@179) that, on a
  summarized-region node, opens the raw region in the **existing event drill-in pane**
  (`buildEventViewLines` @513) — reusing its scroll/pane rather than inventing a sidebar. The
  content shown is the segment file's `## Raw region` for that node's turn range.

## What we build (summary)

1. **Widened `SegmentSink`** (D1) + the concrete filesystem sink implementing it.
2. **Per-session directory** keyed by root `AgentNode.ID` (D2); `internal/session/log.go`
   restructure; flat logs left read-compatible.
3. **Segment files + `index.json`** (D3).
4. **`segment_read` tool** (D4), auto-wired via `DefaultTools()`; child-wiring re-verified by
   grep.
5. **Parameterized + segment GC** at 14 days, symlink-safe (D5).
6. **TUI indicator + `s` "show original"** (D6).
7. **Amend #0027's spec** to record the widened `SegmentRegion` seam (coordination).

## Out of scope

- **`fuse replay` command** — this change stores the data only; replay is a follow-up.
- **Segment encryption** — filesystem permissions suffice for local use.
- **Segment compression** — raw markdown; storage cost is modest (KBs/session).
- **Cross-session `segment_read`** — a session reads only its own segments.
- **Relevance-based candidate selection / read-file dedup** — those are #0028 / #0029.
- **Migrating existing flat logs into the dir layout** — old logs stay flat.

## Tests

- **`Archive` round-trip**: a `SegmentRegion` writes one `<turnStart>-<turnEnd>-<seq>.md` with
  the front-matter + `## Summary` + `## Raw region`, appends one `index.json` entry, and returns
  the absolute pointer. The no-op default sink returns `("", nil)` and writes nothing.
- **Recovery pointer wiring** (against #0027's rule): a real sink's non-empty pointer makes the
  summary carry the "grep your past at `<path>`" line; the no-op sink omits it. (May live as a
  #0027 test once the widened seam lands there.)
- **`segment_read`**: single turn (`"12"`) and range (`"12-18"`) resolve overlapping segments;
  `tool_filter` narrows to matching `Name`; oversized selection returns the bounded slice + the
  "narrow turn_range" trailer; a range with no segments returns a clean "no segments" result,
  not an error.
- **Per-session dir (D2)**: a new session creates `~/.fuse/sessions/<root-id>/session.jsonl` +
  `segments/`; an existing flat `*.jsonl` still loads.
- **GC (D5)**: a >14-day segment file is swept and its `index.json` entry removed; a ≤14-day one
  is kept; a symlinked session dir is descended (not skipped); the 7-day `*.jsonl` log sweep is
  unchanged.
- **TUI (D6)**: the indicator renders when segments exist and is absent when they don't; the `s`
  keypress opens the raw region in the drill-in pane.
- **Child tool wiring** (`patch-every-cloned-child-builder`): `segment_read` is present in the
  registries built at each site enumerated by grep at build time.
- **Real-binary verification** (`verify-tool-loop-at-gateway-seam`): drive the real binary
  against a scripted `LLM_GATEWAY_URL` double that forces a compaction; assert (a) a segment
  file + `index.json` entry are written under the session dir, (b) the post-compaction request
  carries the summary with the recovery pointer, and (c) a subsequent `segment_read` call
  round-trips the raw content — exercising the loop wiring, not just a unit.

## Risks & mitigations

- **#0027 not yet built / seam drift** — the dominant risk. Mitigated by amending #0027's spec
  now to record the widened `SegmentRegion`, and by the reconcile pass re-validating the seam at
  #0030 build time. If #0027 ships the un-widened seam, #0030 either adapts the older signature
  or the widening rides as a small #0027 follow-up — the reconcile log records which.
- **Session-id timing** — the root `AgentNode.ID` must be available when the logger opens; if
  not cleanly threadable, create the session dir lazily on first log/segment write. Verify at
  build.
- **`index.json` concurrent writers** — single-writer within a session assumed; if the
  architecture allows concurrent archives, guard the read-modify-write. Verify at build.
- **TUI segment metadata source** — reading `index.json` on every header render could be
  chatty; prefer a compaction event the loop already emits, falling back to a cached read.
  Decide at build.

## Compression & non-destructive GC (scope expansion, PR #41)

A correction to this design's own choices: the age sweeps DELETED data
(`os.Remove`) and segments were stored UNCOMPRESSED. This expansion makes all
archival gzip-compressed, non-destructive, and self-describing, and gates it with
an antagonistic answer-quality suite proving no degradation when recovered data
lives in a gzipped archive.

### Design

- **Born-compressed segments (creation-time compression).** `FSSegmentSink`
  writes `RenderSegment` output through `gzip` to `<n>.md.gz` (0o600); the index
  records the on-disk `.md.gz` name. `index.json` itself stays UNCOMPRESSED (it
  is small and scanned on every `segment_read`). `LoadSegment` gunzips
  transparently by sniffing the gzip magic (0x1f 0x8b) and falls back from a bare
  `.md` path to `<path>.gz`, so old plaintext and new compressed segments both
  load through the one seam. The lossless JSON raw-region format inside the file
  is untouched — gzip wraps the whole rendered file.
- **`internal/archive` helper.** Domain-agnostic `Archive(path, MetaFunc)` /
  `Open(path)`. `Archive` gzips to `<path>.gz`, writes a `<path>.gz.meta.yml` YAML
  sidecar describing WHAT is in the file (common: `archived_at`, `original_name`,
  `original_bytes`, `compressed_bytes`; plus caller domain fields), then removes
  the original. Idempotent. `Open` reads plaintext or gzip transparently. It
  imports neither `session` nor `tools` (no cycle) — each caller supplies a
  `MetaFunc`.
- **Sidecar frontmatter.** Session logs: `entry_count`, `first_ts`, `last_ts`,
  `node_ids`, `root_label`, `max_depth`, `kinds`. Spill: `tool_name`,
  `created_unix`, `head` preview.
- **Three sweeps converted delete → gzip-archive.** `session.SweepOld` (logs,
  7d) and `tools.sweepSpillDir` (spill, 7d): born plaintext, sweep gzips them at
  the current horizon. `session.SweepOldSegments` (14d): segments are born
  `.md.gz`, so this becomes a non-destructive back-compat bridge that only
  compresses legacy plaintext `.md` in place and re-points the index Path — it
  never deletes or index-prunes, and there is no second destructive horizon.
- **`read_file` gzip-transparent.** Opens via `archive.Open`; the binary-refusal
  guard runs on decompressed bytes; the spill recovery hint resolves after
  archival.

### Policy (documented in ADR-0020)

Segments are born compressed (creation-time), not sweep-compressed. The index
stays uncompressed. Data is retained indefinitely, merely compressed — no
destructive GC horizon.

## Follow-ups (not this change)

- `fuse replay` built on the segment store + session log.
- Tuning the 14-day GC window / output caps to config if usage shows a need.
- A separate optional long-horizon PURGE for truly-ancient `.gz` archives, if
  disk usage ever warrants it (deliberately not built here).
