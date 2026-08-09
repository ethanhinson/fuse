<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0030 — Segment store — pre-compaction transcript archive for replay](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0030-segment-store.md)**
<!-- docket:backlink:end -->

# Segment store — pre-compaction transcript archive — results
Change: #30 · Branch: feat/segment-store · PR: <pending> · Plan: docs/superpowers/plans/2026-08-09-segment-store-plan.md · ADRs: 17, 18, 19

## Verify (human)

Automated tests cover the storage/read/GC/wiring paths, and a real-adapter
gateway-seam test exercises the cmd/fuse sink wiring end-to-end. The checks
below are optional confidence spot-checks in a real interactive session at the
merge gate.

- [ ] In a long `fuse shell` session that triggers a real compaction, confirm a
      segment file + `index.json` appear under `~/.fuse/sessions/<root-id>/segments/`
      and that the injected summary carries the `Recovery: grep your past at <path>`
      line.
- [ ] Call `segment_read` for a summarized turn range (and with a `tool_filter`)
      and confirm it round-trips the raw content, including content whose lines
      look like `key:` or `ls -la:` (the lossless-JSON fix).
- [ ] In the agents tab detail view, confirm the "N segments · before→after"
      indicator renders on a summarized node and that `s` opens the raw region in
      the drill-in pane (sanitized, no pane shear).
- [ ] Confirm one-shot (`fuse "<task>"`), research-probe, and mcp-server paths
      still archive nothing (no per-session dir created) — only the interactive
      shell wires the real sink.

## Findings

- **Reconcile found the seam already built (scope narrowed).** #0027 shipped the
  widened struct-based `SegmentSink` seam (`SegmentRegion` + `Archive(r SegmentRegion)`),
  the `noopSegmentSink` default, and the recovery-pointer line — so the spec's
  "widen the seam" (D1) and "amend #0027's spec" (item 7) work dropped from
  scope. This change implements only the concrete sink + tool + GC + TUI against
  the existing seam. Two anticipated gaps were folded in: populating
  `TurnStart/TurnEnd` at the loop.go invocation (via the existing `turnIndices`),
  and the session-id-timing fallback (root `AgentNode.ID` is unknown at
  `NewLogger` time).
- **Import-cycle avoidance → package split (ADR-0017).** `segment_read` (in
  `internal/tools`) must read segments; the concrete sink must import
  `internal/agent`. A single package would form `tools → segment → agent →
  tools`. Resolved by keeping `internal/segment` agent-free (schema + reader) and
  putting the agent-dependent sink in `internal/segment/fssink`, imported only by
  `cmd/fuse`. A future refactor collapsing these reintroduces the cycle.
- **Per-session directory layout (ADR-0018).** Session logs move to
  `~/.fuse/sessions/<root-id>/session.jsonl` alongside `segments/`; the logger now
  opens after the agent tree exists so the root id is known. Flat legacy `*.jsonl`
  logs stay read-compatible and swept; no migration.
- **Process-global sink holder (ADR-0019).** Sink injection uses a package-level
  `activeSegmentSink` holder in `cmd/fuse`, mirroring `SetSpillDir`/`SetSegmentsDir`.
  A first pass shipped it unsynchronized; the whole-branch review caught the
  inconsistency (siblings are RWMutex-guarded and it is read on child-spawn
  goroutines) and it was fixed to use an RWMutex. Rests on the
  one-process-one-session invariant.
- **Review-caught data-integrity bug — raw-region round-trip was lossy.** The
  first-pass `## Raw region` used `<role> [<name>]:` header lines with verbatim
  content; any body line matching that shape (`ls -la:`, YAML keys, echoed
  recovery lines) was mis-parsed as a message boundary, corrupting `segment_read`
  + `tool_filter` and the TUI drill-in. Fixed by storing the region as a fenced
  JSON array of `model.Message` (self-delimiting, byte-exact round-trip); the
  human-readable `RenderRawRegion` is retained for display only, no longer
  parsed. A round-trip test with header-like/multiline content pins it.

## Follow-ups

- `fuse replay` command built on the segment store + session log (explicitly out
  of scope here; segments carry enough to reconstruct the transcript up to
  summarization). Not auto-captured — `auto_capture` is disabled for this repo;
  noted here for a human to file.

## Notes / deviations

- **Skill-layer degradations (all logged, none fatal).** The resolved
  `plan`, `build`, and `review` skills (`superpowers:*`) were not invocable at
  runtime ("Unknown skill"), so each degraded to the convention's `auto`
  fallback with a prominent warning: the plan was authored directly, the plan was
  executed via a dispatched TDD build worker, and a whole-branch review was
  performed via a dispatched reviewer before the PR. The `docket-status` and
  `docket-adr` composition dispatches ran normally as subagents.
- **Plan deviation — package split.** The plan assumed `FSSegmentSink` could live
  in `internal/segment`; the import cycle forced the `internal/segment/fssink`
  subpackage split (ADR-0017). Same import-boundary intent, one extra subpackage.
- **Plan deviation — sink injection.** Used the package-level `activeSegmentSink`
  holder (ADR-0019) rather than threading a new param through the builder
  signatures, matching the existing `SetSpillDir` pattern.

## Scope expansion — compression & non-destructive GC (PR #41 follow-up)

A fundamental correction to #0030's own design: the age-based sweeps were
DESTRUCTIVE (`os.Remove`) and segments were stored UNCOMPRESSED. This expansion
makes all archival gzip-compressed, NON-DESTRUCTIVE, and self-describing (YAML
metadata sidecars), and proves — with an antagonistic answer-quality suite — that
data recovered from a gzipped archive yields the SAME answer as the uncompressed
original.

### What shipped

- **Born-compressed segments.** `FSSegmentSink.Archive` now writes
  `RenderSegment` output through gzip to `<n>.md.gz`; `index.json` records the
  on-disk `.md.gz` name and itself stays uncompressed (small, must be scanned).
  `segment.LoadSegment` sniffs the gzip magic (0x1f 0x8b) and gunzips
  transparently, and falls back from a bare `.md` path to `<path>.gz` — so old
  plaintext segments and new compressed ones both load through the one seam.
  `segment_read`, the TUI show-original drill-in, and the index all inherit
  compression unchanged. The lossless JSON raw-region format inside the file is
  untouched (gzip wraps the whole rendered file).
- **`internal/archive` helper.** A domain-agnostic gzip archiver
  (`Archive`/`Open` + a caller-supplied `MetaFunc`) used by the log and spill
  sweeps; it imports neither `session` nor `tools` (no cycle). `Archive` gzips a
  file to `<path>.gz`, writes a `<path>.gz.meta.yml` sidecar (common fields:
  `archived_at`, `original_name`, `original_bytes`, `compressed_bytes`; plus the
  caller's domain fields), then removes the original. Idempotent (no-op on `.gz`
  or when the `.gz` already exists). `Open` reads plaintext or gzip transparently
  and falls back to the `.gz` form.
- **All three sweeps converted delete → gzip-archive.**
  - `session.SweepOld` (7-day `*.jsonl`): gzips with session-domain sidecar
    (`entry_count`, `first_ts`, `last_ts`, `node_ids`, `root_label`, `max_depth`,
    `kinds`), skips already-`.gz`.
  - `session.SweepOldSegments` (14-day): repurposed as a non-destructive
    back-compat bridge. Segments are born `.md.gz`, so it only retroactively
    compresses LEGACY plaintext `.md` in place and re-points the index Path —
    segments are never deleted or index-pruned; born-compressed ones are skipped.
    No second destructive horizon (see ADR-0020).
  - `tools.sweepSpillDir` (7-day `*.txt`): gzips with spill-domain sidecar
    (`tool_name`, `created_unix`, `head` preview), skips `.gz`/`.meta.yml`.
- **`read_file` gzip-transparent.** `tools.read` opens targets through
  `archive.Open`, so a `.gz` (or a bare path now backed by `<path>.gz`) is
  gunzipped; the binary-refusal guard runs on the DECOMPRESSED bytes (a gzipped
  binary is still refused). The spill recovery hint resolves after archival.

### Antagonistic answer-quality test results (all PASS)

- **Tier 1 (round-trip).** `internal/archive`: `TestArchiveThenOpenRoundTrip`,
  `TestOpenHandGzippedGoWriter`, `TestOpenSystemGzipBinary` (uses the SYSTEM
  `gzip` binary; skips if absent), `TestOpenPlaintextBackCompat`,
  `TestArchiveSidecarCommonFields`.
- **Tier 2 (recovery-tool parity).** `internal/tools`:
  `TestSegmentReadMixedCompressionParity` — half `.md` / half `.md.gz`, a
  `segment_read` spanning both recovers identical text. `internal/segment`:
  `TestLoadSegmentGzipTransparent`, `TestLoadSegmentGzFallbackByPath`.
- **Tier 3 (read_file/grep parity).** `internal/tools`:
  `TestReadFileTransparentGunzip`, `TestReadFileGzFallbackByBasePath`,
  `TestSpillRecoveryHintResolvesAfterArchival`.
- **Tier 4 (adversarial end-to-end — THE GATE).** `cmd/fuse`:
  `TestAdversarialGzVsPlaintextParity` — the needle
  ("the API key rotation interval is 4217 seconds") lives ONLY in an archived,
  pruned-from-context segment; the real `a.Run` loop must call `segment_read` to
  recover it. The gz arm and the plaintext control arm produce the IDENTICAL
  final answer (4217); any divergence fails, with an anti-vacuity guard that the
  needle actually reached the model. `TestAdversarialSystemGzipParity` — same,
  archive made by the SYSTEM gzip binary.
- **Tier 5 (corruption/edge).** Truncated `.gz` → graceful error, never panic
  (`archive`, `segment`, `read_file`); empty file; gzipped-binary still refused;
  idempotency (archiving an already-`.gz` file is a no-op).

### Compression ratios observed on real data

Measured against the developer's own `~/.fuse` corpus:

- Session logs (`*.jsonl`), 62 files: 92,011 → 20,062 bytes — **4.59x (78% saved)**.
- Tool-output spill (`*.txt`), 262 files: 10,079,669 → 3,395,109 bytes — **2.97x
  (66% saved)**.
- Segments (representative rendered region, mostly repeated JSON tool output):
  ~**100x+** — highly compressible; a 30 KB synthetic region compressed 119x.

### New architectural decision

- **ADR-0020** — born-compressed segments (creation-time, not sweep-time), the
  index stays uncompressed, and no second destructive GC horizon (retain
  indefinitely, merely compressed).
