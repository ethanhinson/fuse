---
id: 4
slug: byo-search-key-brave-primary-provider-resolution
title: BYO search key with Brave-primary provider resolution and a config-driven custom HTTP provider
status: Accepted
date: 2026-08-05
supersedes: []
reverses: []
relates_to: []
change: 14
---

## Context

fuse's research mode needs a web search backend, but fuse has no vendor in the middle —
models arrive via a LiteLLM gateway, and none of them operate a search backend. So unlike
hosted assistants where the vendor holds a search key server-side and re-bills, the fuse
user must supply their own search key. There are many search APIs; picking one primary and
defining a clean extension story matters. Evidence gathered during grooming: the web search
behind some hosted assistants is, by public evidence, the Brave Search engine (the vendor
holds the Brave key server-side and re-bills). fuse users can use the same engine directly
at a lower effective price by bringing their own key.

## Decision

Ship three `SearchProvider` adapters in v1 — Brave (primary), Tavily, and a config-driven
`CustomHTTPProvider` — with a fail-loud resolution order and NO zero-config keyless default.

Resolution order (first match wins):

1. explicit `research.provider` config
2. `BRAVE_SEARCH_API_KEY` env
3. `TAVILY_API_KEY` env
4. `[research.custom]` block, if its `url` is set
5. otherwise a loud error naming every setup path

Resolution happens at first use; there are no silent fallbacks at query time. The
`CustomHTTPProvider` is the user-land extension point: rather than shipping a per-engine
SearXNG adapter, it is driven entirely by config (a url template plus JSON field mappings
whose defaults match SearXNG's `format=json` shape), so self-hosters plug in any
JSON-speaking search endpoint without a fuse release. Exa and MCP-search adapters are
deferred to future changes.

## Consequences

- Enables users to use the same Brave engine directly (BYO key) at lower cost, a hosted
  alternative (Tavily, which also returns extracted content), and any self-hosted/JSON
  endpoint via config alone.
- Fail-loud resolution means a misconfiguration surfaces immediately with actionable
  guidance, never a silent wrong-provider query.
- Cost / trade-off: no zero-config experience — the user must configure at least one key or
  a custom URL before `/research` works.
- fuse must document Brave attribution (a README line) to satisfy Brave's free-credit terms.
