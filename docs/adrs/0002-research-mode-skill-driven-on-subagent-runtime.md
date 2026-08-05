---
id: 2
slug: research-mode-skill-driven-on-subagent-runtime
title: Research mode is skill-driven on the subagent runtime, not Go-orchestrated
status: Accepted
date: 2026-08-05
supersedes: []
reverses: []
relates_to: []
change: 14
---

## Context

fuse needs a web-research capability: diversify a question into facets, search and
fetch across those facets, and synthesize a cited report. The killed change 0011 had
proposed a bespoke Go `ResearchOrchestrator` — goroutine fan-out, Go query-generation,
and a Go synthesizer. Meanwhile, change 0012 shipped a hardened subagent runtime:
parallel `spawn_agent` batches, width capping, slot-yield-while-blocked, context
management, tracing, and the TUI agent tree. Building a second orchestration mechanism
in Go for research would duplicate all of that concurrency and control machinery.

## Decision

The research flow lives in a markdown skill executed by the MODEL, which fans out via
parallel `spawn_agent` batches on the existing 0012 runtime. Go ships only two
primitives — the `web_search` and `web_fetch` tools plus the three search-provider
adapters — and the embedded research skill. There is no new Go orchestration,
query-generation, or synthesis code. The model diversifies the question into 4-5
facets, spawns one subagent per facet (each running `web_search` then `web_fetch`),
dedups sources by URL, and synthesizes one cited report.

## Consequences

- Enables reuse of 0012's width-capping, slot-yield, tracing, and agent-tree for free —
  no duplicated concurrency code.
- The flow is tunable by editing prompt text without recompiling.
- Zero new concurrency code to maintain.
- Cost / trade-off: the run shape (facet count, dedup discipline, citation format) is
  PROMPT-enforced, not code-enforced, so it cannot be unit-tested end-to-end. The Go
  primitives are unit-tested; the flow itself is verified manually via the agent-tree
  TUI.
- Mirrors how some assistants run WebSearch/WebFetch in secondary conversations to keep
  the main context clean.
