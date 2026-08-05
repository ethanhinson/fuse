---
id: 29
slug: read-file-dedup-cache
title: Read_file content deduplication cache
status: proposed
priority: medium
type: feat
created: 2026-08-06
updated: 2026-08-06
depends_on: [12]
related: [27]
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
| Artifact | Link |
|---|---|
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
