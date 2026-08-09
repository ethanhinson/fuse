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
