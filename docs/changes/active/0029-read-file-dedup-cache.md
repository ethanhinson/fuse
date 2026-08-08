---
id: 29
slug: read-file-dedup-cache
title: Read_file content deduplication cache
status: deferred
priority: medium
type: feat
created: 2026-08-06
updated: 2026-08-08
depends_on: [12]
related: [27, 28]
discovered_from: [27]
adrs: []
spec:
plan:
results:
trivial: false
auto_groomable:
branch:
pr:
blocked_by:
reconciled: false
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
<!-- docket:artifacts:end -->

## Why

In long agent sessions, the model frequently reads the same file multiple times — a config file, a key source file, a spec document. Each read consumes context tokens for the full file content. The design doc (`docs/designs/context-management.md`) estimates that deduplicating repeated file reads could save ~30% of tool-result token usage. fuse already has the infrastructure for this: the `read_file` tool could cache file content by (inode, mtime, size) fingerprint and return a summary or a pointer ("already read in the current session — see turn 3") instead of the full content. This saves tokens without losing information, because the full content is still in the context (from the first read) or recoverable from the spill directory.

## What changes

- **File fingerprint cache** in `internal/tools/read.go`: a bounded LRU cache (configurable size, default 50 entries) keyed by `(device, inode, mtime, size)` — computed via `os.Stat` before reading. If the same file was read in the current session and its content hasn't changed, return a compact reference instead of the full content.
- **Cache result format**: when a cached read is detected, instead of the full file content, return:
  ```
  [file already read at turn 3 / tool call 2 — 847 lines shown]
  [full content preserved in context/spill — use grep for specific patterns]
  [file fingerprint: dev=16777234 ino=9876543 mtime=1722880000 size=28473]
  ```
- **Line-range awareness**: if the previous read was for lines 1-1000 and the new read is for lines 500-1500, the cache identifies the overlap and returns only the delta (lines 1001-1500), prefixed by overlap count. This handles the common case of "show me more context around line X."
- **Session scoping**: the cache is bound to the agent tree's lifetime — cleared between user turns (or configurable persistence for multi-turn conversations). A `fresh: true` parameter on `read_file` bypasses the cache and forces a fresh read.
- **Spill directory integration**: if the file was spilled (over 20KB), the cached reference points to the spill file path rather than re-reading.

## Out of scope

- Cross-session cache persistence — in-memory only.
- Dedup for `bash cat` or `grep` output — `read_file` tool only.
- Content-hash comparison for identical-content-but-different-inode files (e.g. re-created files with same content) — mtime + size is sufficient for the common case.

## Research notes (input for the brainstorm)

The 30% savings estimate comes from observed agent behavior: models often read the same file multiple times because (a) the context scrolled past the first read, (b) they need to verify a detail, or (c) a spawned subagent re-reads a file the parent already read. The (device, inode, mtime, size) fingerprint is the standard Unix file identity check — it's fast (just `os.Stat`), handles renames, and detects modifications. The LRU size of 50 covers the common case of a codebase session (maybe 10-20 unique files read, often multiple times). The line-range overlap optimization is the subtler part: if a model read lines 1-100 and then reads lines 50-150, the cache should return "lines 50-100 already shown, here are lines 101-150" — this requires tracking `(file, start, end)` tuples per read, not just file identity. The challenge is that the model might have forgotten the earlier content even if it's still in context — the reference format is designed to let the model decide whether to re-read or trust the cache hint.

## Why deferred

Deferred on 2026-08-08 during grooming, pending #0027 (anchored summarization) and #0028 (semantic relevance scoring) landing — this change **overlaps heavily** with both, and building it speculatively risks a redundant, overlapping context-savings system.

**The overlap.** A duplicate `read_file` result is precisely the kind of low-value, redundant tool result that #0028's relevance pruning is designed to stub (its dependency-reuse and recency signals rank an older identical copy as prunable), and that #0027's summarization compresses 5–10× once the copy scrolls into the old region. Both attack the same cost #0029 targets — too many tool-result tokens in history — but more generally. #0029 is, roughly, a hash-exact special case of what #0028 does by score.

**The genuine residual #0029 might still own** (what to measure before reviving):
- **Proactive vs reactive.** #0027/#0028 only fire when *over budget* (85% threshold). #0029 prevents the duplicate body from *ever entering* history, at read time, regardless of budget pressure — the only one of the three that reduces cost *below* the threshold.
- **Correctness coupling discovered in brainstorm.** A safe dedup cannot be a dumb independent LRU: a reference to already-pruned content is a dangling pointer (worse than no dedup). The safe design is **presence-gated** — dedup to a reference only when the identical content (SHA-256 of the bytes, *not* mtime+size, which can lie after a git checkout) at a same-or-narrower line range is still present and unpruned in context, live-invalidated when `pruneOldToolResults` stubs its backing message; always naming a spill/grep recovery path; `fresh: true` forces a real read. This coupling is what makes #0029 *not* trivially cheap — another reason to confirm it's worth it before building.

**Revival criteria (data-driven, not a guess).** When #0027 and #0028 have landed, instrument the loop to measure, over representative sessions:
1. **Duplicate-read token cost after #0028** — how many tokens of history are still attributable to redundant identical `read_file` bodies that #0028's relevance pruning did *not* stub. If ~0, **kill #0029** — relevance scoring already ate it.
2. **Below-threshold residual** — how much duplicate-read cost accrues while the session is *under* the 85% prune threshold (the window #0027/#0028 never touch). A material figure here is the strongest case to build #0029 as the proactive complement.
3. **Subagent re-reads** — whether cross-agent duplicate reads (child re-reading a parent's file) are a real cost; note that agent trees have *separate* registries/histories today, so catching these needs shared cache state (a larger design) — this sub-benefit may not survive even if #0029 revives.

Revive to `proposed` (needs-brainstorm) only if (1) shows a material residual or (2) shows a material below-threshold cost; otherwise kill. The brainstorm's presence-gated safety model and SHA-256 keying are recorded here so a revival starts from the settled design, not from the stub's weaker mtime+size fingerprint.
