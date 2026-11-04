---
id: 81
slug: 'resume-is-a-full-fold-of-the-stream-orphans-are-failed-durab'
title: 'Resume is a full fold of the stream; orphans are failed durably, never silently dropped or retried'
status: 'Accepted'
date: '2026-11-04'
supersedes: []
reverses: []
relates_to: []
change: 127
---

## Context

When a loop moves between replicas, the resuming runtime replays the durable event stream. The original fold handled only four transcript kinds and did not filter by node, so child turns bled into the root transcript, budget counters reset to zero, and a child mid-flight when the owner died left an assistant message with unanswered tool calls.

## Decision

reconstructMessages is replaced by reconstructLoopState, a full fold that filters by NodeID, builds a tree view, seeds budgets from the stream, and carries summary text. Orphaned children and unanswered root tool calls are repaired on resume by appending synthetic spawn.done and tool.result events to the durable stream before the fresh agent launches. Orphans are marked done-with-error, never dropped or retried.

## Consequences

A resumed loop is identical to one that never moved. The transcript is API-valid, budgets reflect pre-resume spend, and the summary carries forward. Synthetic events are permanent stream records. Cost is one extra replay after repair, only when orphans exist.

## Alternatives considered

Retry orphans (unsafe, non-idempotent), silently drop (invalid API request), let provider reject (resume unusable after death), checkpoint in-memory state (larger change, stream is source of truth).
