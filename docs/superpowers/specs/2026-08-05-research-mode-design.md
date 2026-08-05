<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0014 — Research Mode — Web Search, Fetch & Cited Synthesis on the Subagent Runtime](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0014-research-mode.md)**
<!-- docket:backlink:end -->

# Research Mode — Web Search, Fetch & Cited Synthesis on the Subagent Runtime

**Spec for change 0014** · groomed 2026-08-05 (interactive brainstorm)

---

## Overview

`/research <query>` produces a cited research report by driving the **existing 0012
subagent runtime** with two new Go tools and one embedded skill. The model — not Go —
orchestrates: it diversifies the question into 4–5 search facets, spawns one subagent
per facet (search → fetch), and synthesizes a cited report. Go ships exactly two
primitives (`web_search`, `web_fetch`) and the markdown skill that directs the flow.

This is the salvage of killed change 0011: its ecosystem survey, Brave/provider
research, scraper design, and citation format carry over; its bespoke
`ResearchOrchestrator`, `query_gen.go`, and `synthesizer.go` are **deliberately not
built** — the 0012 runtime plus the model replace all three.

## Decisions (settled in the groom, with rationale)

### D1 — Skill-driven, not Go-orchestrated

The research flow lives in a markdown skill executed by the model, which fans out via
parallel `spawn_agent` batches. Zero new orchestration code; the 0012 runtime provides
width capping, slot yielding, context management, tracing, and the TUI agent tree for
free. The flow is tunable without recompiling. Accepted trade-off: run shape
(facet count, dedup discipline, citation format) is prompt-enforced, not code-enforced.

This mirrors what Claude Code actually does: its WebSearch/WebFetch tools run
search+fetch in secondary conversations to keep the main context clean — the same
shape as one subagent per facet.

### D2 — The skill is an embedded built-in

The research skill markdown ships inside the fuse binary (`go:embed`) and registers
with the 0004 skill runtime at startup. `/research` works with no user setup beyond
the Brave key; a user skill of the same name shadows the embedded one (standard
skill-runtime precedence).

### D3 — Brave primary + Tavily in v1; user-extensible beyond that

Evidence gathered during the groom: Claude's web search is Anthropic's **server-side**
API tool (`web_search_20250305`, $10/1k searches, results returned encrypted), and its
engine is by all evidence **Brave** — Brave appeared on Anthropic's subprocessor list
2025-03-19, Simon Willison found `BraveSearchParams` in the tool schema and identical
citations between Claude and Brave. Claude users never set a search key only because
Anthropic holds the Brave key server-side and re-bills through the Anthropic bill.

fuse has no vendor in the middle (models arrive via LiteLLM; none operate a search
backend), so the user holds the key: **BYO `BRAVE_SEARCH_API_KEY`** — same engine
Claude uses, $5/1k direct (vs $10/1k re-billed), first ~$5/month free via Brave's
credit. Signup: api-dashboard.search.brave.com (card required; the free *tier* was
retired 2026-02, replaced by $5/month free *credits*). Free-credit attribution
requirement is satisfied by a Brave Search line in fuse's README.

v1 ships two hosted adapters — **Brave** (primary) and **Tavily** — plus a
config-driven **custom HTTP provider** (D6) as the user-land extension point, so a
self-hosted engine like SearXNG needs configuration, not a fuse release. Exa and
MCP-search remain future changes. **Provider resolution:** explicit
`research.provider` config → `BRAVE_SEARCH_API_KEY` env → `TAVILY_API_KEY` env →
`[research.custom]` if configured → loud error naming all setup paths. Resolution
happens at first use and fails loudly; no silent fallbacks at query time.

### D4 — robots.txt on by default, config override

`web_fetch` checks robots.txt per host (fetched once, cached per session) and refuses
disallowed URLs with a note in the tool result. `research.respect_robots: false`
overrides for dev/testing.

### D5 — No local result cache

Dropped from scope (was a 0011 dev convenience). File a follow-up stub only if
re-fetch pain shows up in practice.

### D6 — User-land extensibility via a generic HTTP provider (amended post-groom, 2026-08-05)

Rather than shipping a per-engine SearXNG adapter, v1 ships `CustomHTTPProvider` — a
`SearchProvider` driven entirely by config: a URL template plus JSON field mappings.
Users opt into SearXNG (or any JSON-speaking search endpoint) in user land; fuse
ships no engine-specific code. The field-mapping defaults match SearXNG's
`/search?format=json` response shape, so a SearXNG user sets only the URL.

## Components

### `internal/research/provider.go` — interface (carried from 0011)

```go
type SearchResult struct {
    Title   string
    URL     string
    Snippet string
}

type SearchProvider interface {
    Search(ctx context.Context, query string, maxResults int) ([]SearchResult, error)
    Name() string
}
```

### `internal/research/brave.go` — BraveSearchProvider

- `GET https://api.search.brave.com/res/v1/web/search?q=...&count=N` with
  `X-Subscription-Token` header; JSON response → `[]SearchResult`.
- Snippet-only (no page content) — `web_fetch` supplies content, exactly the
  WebSearch/WebFetch split Claude Code has.
- Bounded per the bound-every-model-call learning's discipline generalized to HTTP:
  per-attempt timeout, bounded retries on 429/5xx honoring `Retry-After`, and a
  labeled trace entry.
- **Build-time evaluation:** Brave's newer "LLM Context" endpoint returns
  AI-optimized content and could reduce fetch volume; evaluate when building, adopt
  only if it demonstrably beats web-endpoint + readability extraction.

### `internal/research/tavily.go` — TavilyProvider

- `POST https://api.tavily.com/search` (Bearer `TAVILY_API_KEY`) with
  `{query, max_results}`; JSON response → `[]SearchResult`.
- Tavily returns extracted page content alongside results; when present it seeds
  `Snippet`, letting the skill skip `web_fetch` for sources whose returned content
  already suffices.
- Same HTTP bounds/retry/trace discipline as Brave.

### `internal/research/custom.go` — CustomHTTPProvider (user-land extension point)

- Driven by `[research.custom]`: a `url` template with `{query}`/`{count}`
  placeholders, optional `headers` (for instances behind auth), and JSON field
  mappings — `results_path`, `title_field`, `url_field`, `snippet_field` —
  defaulting to SearXNG's `format=json` shape (`results[]` / `title` / `url` /
  `content`).
- GET, JSON responses only; same bounds/retry/trace discipline as the hosted
  adapters. Selected explicitly (`research.provider: custom`) or as the last
  auto-resolution step when the block is configured.
- README documents the SearXNG example: only
  `url: "https://searx.example.com/search?q={query}&format=json"` is needed.

### `internal/research/scraper.go` — fetch + extraction (carried from 0011)

1. HTTP GET, 10 s per-URL deadline; robots.txt gate per D4.
2. `text/html` → strip script/style/nav/header/footer/aside →
   `go-shiori/go-readability` main-body extraction → tag-strip fallback (always
   produces something). Non-HTML (PDF etc.) skipped with a note in the tool result.
3. Word-boundary truncation at `research.max_content_kb` (default 50).
4. Token-bucket rate limit: 2 req/s per domain.

### `internal/tools/web_search.go`, `internal/tools/web_fetch.go` — tool registration

- Registered as ordinary built-ins in the Registry → available to the main agent and
  to subagents via `Registry.Subset`; the research skill force-includes both.
- **Permission posture:** both are network reads; they go through the PermissionGate
  like any tool (session-allow works). Default policy left to the existing 3-source
  merge; users may safe-list in config. One-shot mode auto-approves as usual.
- **Sanitization:** fetched web content is hostile input — tool output goes through
  the 0012 sanitization path (strip ESC/C0/C1/CR, tab expansion, NUL-sniff) before
  TUI display, per the sanitize-untrusted-bytes learning. Content handed to the model
  is plain extracted text.
- Large outputs ride 0012's spill-file truncation; no new caps invented here.

### Embedded skill — `research`

The flow prose, at minimum directing the model to:

1. Reformulate the question into 4–5 facets covering: direct answer, background,
   recent developments, counterarguments/caveats, practical examples.
2. Spawn one subagent per facet **in a single parallel batch** (runtime width cap
   applies; queued children stay visible). Each child: `web_search` → pick the most
   promising results → `web_fetch` each → return per-facet findings **with URLs**.
3. Deduplicate sources by URL across facets in the parent.
4. Synthesize one report: executive summary, titled sections, `[N]` citation markers,
   numbered source list. Rendered as normal markdown in the transcript (0006
   markdown rendering; no new TUI work).

Nested spawn depth stays at 1 for research (children do not spawn); the runtime's
slot-yield behavior (slot-cap learning) applies regardless.

### `/research <query>` — slash built-in (change 0010 dispatch)

Thin: validates a non-empty query, resolves the provider (fail loud per D3), and
invokes the research skill with the query. No Go pipeline behind it.

### Config — `[research]` in fuse config

```go
type ResearchConfig struct {
    Provider      string // "brave" | "tavily" | "custom" | "" (auto: Brave env → Tavily env → custom)
    MaxQueries    int    // default 5 (facet count hint passed to the skill)
    MaxResults    int    // default 5 (per web_search call)
    MaxContentKB  int    // default 50 (web_fetch truncation)
    RespectRobots bool   // default true
    Custom        CustomProviderConfig // [research.custom] — see D6
}

type CustomProviderConfig struct {
    URL          string            // template with {query} / {count}; empty = not configured
    Headers      map[string]string // optional
    ResultsPath  string            // default "results" (SearXNG shape)
    TitleField   string            // default "title"
    URLField     string            // default "url"
    SnippetField string            // default "content"
}
```

## New / modified files

| Path | Purpose |
|---|---|
| `internal/research/provider.go` | `SearchProvider` + `SearchResult` |
| `internal/research/brave.go` | Brave adapter (primary) |
| `internal/research/tavily.go` | Tavily adapter |
| `internal/research/custom.go` | config-driven custom HTTP adapter (user-land extension point) |
| `internal/research/scraper.go` | fetch, robots gate, readability extraction, rate limit |
| `internal/tools/web_search.go` | `web_search` built-in tool |
| `internal/tools/web_fetch.go` | `web_fetch` built-in tool |
| `skills/research.md` (embedded) | the research flow skill |
| `internal/tui/…` (builtin provider) | register `/research` |
| config package | `ResearchConfig` + `[research]` block |
| `README.md` | Brave Search attribution + key setup |

## Testing

- Providers (`Brave`, `Tavily`, `CustomHTTP`): `httptest`-backed — happy path,
  429/`Retry-After`, 5xx retry exhaustion, malformed JSON; custom additionally:
  field-mapping defaults (SearXNG-shaped fixture), overridden mappings, unconfigured
  block excluded from auto-resolution. Test doubles mutex-guarded per the
  mutex-test-double learning.
- Resolution order: config beats env; Brave env beats Tavily env; custom last;
  loud error when nothing resolves.
- Scraper: fixture HTML → extraction; robots-disallowed refusal; override honored;
  non-HTML skip; truncation at boundary; per-domain rate limit.
- Tools: registration, Subset inclusion, sanitized output.
- Skill flow: prompt-driven, so end-to-end shape is verified manually via the agent
  tree TUI rather than unit-tested.

## Out of scope

- Exa and MCP-search adapters (future changes). No dedicated SearXNG adapter —
  self-hosters plug SearXNG in through the custom HTTP provider (D6).
- Any zero-config keyless search path — the custom provider is opt-in
  configuration, not a shipped default.
- Any new orchestration/fan-out mechanism; any Go query-gen or synthesis code.
- PDF/non-HTML extraction; news-specific features; result cache; in-report editing
  or export.

## Open questions (build-time)

1. Whether Brave's LLM Context endpoint replaces some `web_fetch` volume (D3 note) —
   decide from a quick spike during build, not now.
