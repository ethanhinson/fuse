---
id: 14
slug: research-mode
title: Research Mode — Web Search, Fetch & Cited Synthesis on the Subagent Runtime
status: proposed
priority: high
type: feat
created: 2026-08-05
updated: 2026-08-05
depends_on: [12]
related: [10, 11, 12]
discovered_from: [11]
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

Change 0011 (deep-research) was killed because its core deliverable — a bespoke
`ResearchOrchestrator` goroutine fan-out positioned as a "precursor to the subagent
dispatch layer" — was invalidated by change 0012, which shipped a hardened subagent
runtime with parallel `spawn_agent` batches, width capping, slot yielding, and
context management. Building a second orchestration mechanism would be a regression.

The still-valuable remainder of 0011 survives here: fuse has no research capability
today, while every serious harness does. The winning pattern is LLM query
diversification (4–5 reformulated facets, not the user's literal question) fanned
out in parallel, full page content extracted, and synthesized into a cited report.
This change delivers that pattern by driving the EXISTING subagent fan-out instead
of building its own — a much smaller change than 0011.

0011's spec (`docs/superpowers/specs/2026-08-04-deep-research-design.md`, on
`docket`) remains useful background: its ecosystem survey, provider comparison
(Exa/Brave/Tavily/MCP), scraper design, and synthesis format largely carry over.
Its orchestrator and TUI-progress sections are superseded by the 0012 runtime.

## What changes

- Web search tool(s) — `SearchProvider` abstraction with adapters (Exa
  content-included, Brave, Tavily, connected MCP search tools) exposed as
  agent-callable built-ins.
- Web fetch tool — HTTP fetch + readability-style extraction with per-domain rate
  limiting and content truncation, exposed as an agent-callable built-in.
- Query diversification — LLM reformulation of the user's question into 4–5
  targeted search facets.
- Research flow on the subagent runtime — parallel `spawn_agent` batches (one per
  facet: search + fetch), deduplication by URL, single cited-synthesis pass
  producing a `Report` rendered as markdown in the TUI.
- `/research <query>` slash built-in (change #10's dispatch) and a `[research]`
  config block for provider selection and tuning.

## Out of scope

- Any new bespoke orchestration/fan-out mechanism — the 0012 runtime is the only
  dispatch path. If a join helper is needed, it is shaped to the runtime's slot
  yielding (per 0012's notes), not a parallel system.
- PDF and non-HTML content extraction — non-HTML URLs are skipped with a note.
- Real-time / news-specific search features (date filtering, etc.).
- In-report editing or export.

## Open questions

1. How much of the research flow is skill/prompt-driven vs. hard-coded Go (now
   that subagents + tools exist, does research need dedicated Go orchestration at
   all, or is it a skill over the search/fetch tools)?
2. Provider resolution order and whether the MCP-search adapter ships in v1.
3. robots.txt compliance posture and an optional local result cache (carried from
   0011's open questions).

## Reconcile log
