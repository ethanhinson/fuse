---
id: 18
slug: per-session-directory-layout-flat-log-read-compat
title: Per-session directory layout for session logs and segments, with flat-log read-compatibility and no migration
status: Accepted
date: 2026-08-09
supersedes: []
reverses: []
relates_to: [17]
change: 30
---

## Context

Before change #0030, fuse wrote session logs flat: `~/.fuse/sessions/YYYY-MM-DD-XXXXXX.jsonl`. Change #0030 (segment store) needs a stable per-session home for pre-summarization segment files and their `index.json`, keyed by the session identity. The natural key is the root `AgentNode.ID` — the id the agent tree/TUI already renders — but that id is only known AFTER the agent tree is constructed, whereas the logger was previously opened before the tree existed. This creates a startup-ordering tension: the segment store wants to key on-disk state by the root node id, but the logger's open predates the existence of that id.

## Decision

Introduce a per-session directory `~/.fuse/sessions/<session-id>/` where `<session-id>` is the root `AgentNode.ID`, containing `session.jsonl` (the log, moved under the dir) and `segments/` (segment files + `index.json`). The session logger opening is relocated to after the agent tree exists so the root id is available (a new `NewSessionLogger(baseDir, sessionID)` path).

Existing flat `*.jsonl` logs are left in place and stay READ-compatible; there is NO migration of old logs, and the log GC still sweeps the flat files. New sessions use the directory layout. Only the interactive shell session wires the real per-session sink; one-shot/probe/mcp paths keep the no-op sink.

This is the D2 decision from the #0030 spec, made deliberately.

## Consequences

- (+) Segments and log share one per-session home, enabling recovery and the future `fuse replay`; session identity is the existing root node id, so no new id generator is introduced.
- (+) Back-compat preserved: old flat logs still load and are swept by the log GC.
- (−) A real on-disk restructure of `internal/session`; the session logger now opens later in startup (after tree construction) — a startup-ordering constraint future changes must respect.
- (−) Two on-disk shapes coexist (flat legacy + per-session dir), which readers and GC must both handle.
