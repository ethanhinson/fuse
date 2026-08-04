---
id: 11
slug: deep-research
title: Deep Research Mode — Web Search + Fan-out Synthesis
status: proposed
priority: high
type: feat
created: 2026-08-04
updated: 2026-08-04
depends_on: []
related: [10]
discovered_from: []
adrs: []
spec: docs/superpowers/specs/2026-08-04-deep-research-design.md
plan:
results:
trivial: false
auto_groomable: false
branch:
claimed_at:
pr:
blocked_by:
reconciled: false
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-08-04-deep-research-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-08-04-deep-research-design.md) |
<!-- docket:artifacts:end -->

## Why

Every serious AI harness now has a "research" mode that's meaningfully more powerful than a single LLM response: Claude Code's `/research` and ultra modes, OpenCode's MCP-delegated search, Cursor's `@web`. The pattern that works is LLM-generated query diversification (not the user's literal question, but 4–5 reformulated facets) fanned out in parallel across a web search provider, with full page content scraped and synthesized into a cited report. Fuse has no equivalent today.

This also establishes the `internal/research` package as the first explicit subagent-shaped orchestration in the codebase — the goroutine fan-out is a direct precursor to the subagent dispatch layer. Designing this now pins the interface so the subagent upgrade is a clean swap, not a rewrite.

## What changes

- A `SearchProvider` interface + adapters for Exa (content-included), Brave Search, Tavily, and any connected MCP search tool.
- A `Scraper` that does HTTP fetch + readability-style HTML extraction (go-readability), with per-domain rate limiting and content truncation.
- `GenerateQueries` — LLM call that diversifies the user's question into 4–5 targeted search facets.
- `ResearchOrchestrator` — fans out queries in parallel goroutines (subagent dispatch as upgrade path), deduplicates by URL, synthesizes via a single LLM call into a `Report` with citations.
- `/research <query>` slash built-in wired to the TUI (requires change #10), streaming progress and rendering the final report as markdown.
- `[research]` config block in `~/.fuse/config.yml` for provider selection, tuning, and optional local cache.

## Out of scope

- PDF and non-HTML content extraction — URLs returning non-HTML content are skipped with a note.
- Real-time / news-specific search features (date filtering, etc.) — deferred.
- Agentic follow-up queries (research deciding to search again based on gaps) — deferred to subagent layer.
- In-report editing or export — out of scope for this change.

## Open questions

1. **Subagent dependency id**: The subagent architecture change is being designed separately in docket. Once that id is known, add it to `depends_on` here — the implementation should block on that landing first, or at minimum be reviewed alongside it.
2. **robots.txt compliance**: Always-on vs. opt-out flag — see spec.
3. **Local result cache**: Default-off SQLite cache for dev ergonomics — see spec.
