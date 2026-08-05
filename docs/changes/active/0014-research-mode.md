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
spec: docs/superpowers/specs/2026-08-05-research-mode-design.md
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
| Spec | [2026-08-05-research-mode-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-08-05-research-mode-design.md) |
<!-- docket:artifacts:end -->

## Why

Change 0011 (deep-research) was killed because its core deliverable — a bespoke
`ResearchOrchestrator` goroutine fan-out — was invalidated by change 0012, which
shipped a hardened subagent runtime. The still-valuable remainder survives here:
fuse has no research capability today, while every serious harness does. The
winning pattern is LLM query diversification (4–5 reformulated facets) fanned out
in parallel, full page content extracted, and synthesized into a cited report —
delivered by driving the EXISTING subagent fan-out instead of building its own.

Groomed 2026-08-05: the design is **skill-driven** — the model orchestrates via an
embedded skill and parallel `spawn_agent`; Go ships only the search/fetch tools.
Search is **Brave-only** in v1 (the same engine that, per public evidence, powers
Claude's own web search — Anthropic just holds the key server-side and re-bills;
fuse users bring their own `BRAVE_SEARCH_API_KEY` at half the effective price).
Full rationale and evidence in the spec.

## What changes

- `SearchProvider` interface + a single `BraveSearchProvider` adapter (Brave web
  search REST API, `X-Subscription-Token`). Resolution: `research.provider` config
  → `BRAVE_SEARCH_API_KEY` env → loud error naming the setup path.
- `web_fetch` tool — HTTP fetch + readability extraction, robots.txt gate (on by
  default, `research.respect_robots: false` override), per-domain rate limiting,
  word-boundary truncation; and a `web_search` tool over the provider. Both
  ordinary built-ins under the PermissionGate; outputs sanitized before TUI
  display.
- Embedded `research` skill (go:embed, registered with the 0004 skill runtime,
  user-shadowable) directing the flow: diversify into 4–5 facets → one subagent
  per facet (search → fetch) in a parallel batch → dedup by URL → single cited
  synthesis rendered as markdown in the transcript.
- `/research <query>` slash built-in (change 0010 dispatch) — thin: validate,
  resolve provider, invoke the skill.
- `[research]` config block (provider, max_queries, max_results, max_content_kb,
  respect_robots) and a Brave Search attribution/setup note in the README.

## Out of scope

- Other search adapters — Exa, Tavily, SearXNG, MCP-search — and any keyless
  search path (future changes; the interface is the extension point).
- Any new bespoke orchestration/fan-out mechanism, Go query-gen, or Go synthesis
  code — the 0012 runtime plus the model own the flow.
- PDF and non-HTML content extraction — skipped with a note.
- Real-time / news-specific search features; local result cache; in-report
  editing or export.

## Open questions

(Resolved in the 2026-08-05 groom; one build-time question — whether Brave's LLM
Context endpoint replaces some fetch volume — lives in the spec.)

## Reconcile log
