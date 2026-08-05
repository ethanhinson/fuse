<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0011 — Deep Research Mode — Web Search + Fan-out Synthesis](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/archive/2026-08-05-0011-deep-research.md)**
<!-- docket:backlink:end -->

# Deep Research Mode — Web Search + Fan-out Synthesis

**Spec for change 0011**

---

## Overview

Research mode fans out multiple LLM-generated search queries in parallel, scrapes the top results for full content, deduplicates and ranks what it finds, and synthesizes a cited report. The design is informed by a survey of how Claude Code, OpenCode, Cursor/Continue.dev, and the Exa research API approach this problem.

The orchestrator is designed with the subagent layer (a separate docket change, TBD id) in mind: the first implementation uses goroutine-level parallelism as a stable internal interface, with subagent dispatch as the natural upgrade path when that layer lands.

---

## Ecosystem Research

### Claude Code

Claude Code's `/research` and "ultra" modes:
- **WebSearch tool**: SERP-level results (snippet + URL) via a provider abstraction — the agent never talks to a search engine directly.
- **WebFetch tool**: Raw page fetch → DOM stripping (scripts, style, navigation) → text extraction, capped per page.
- **Fan-out pattern**: The orchestrating agent first asks an LLM to generate 3–7 distinct search queries that cover the user's question from different angles. Each query is dispatched as an independent subagent (search + scrape). The parent agent then receives all results and runs a final synthesis pass.
- **Query diversification** is the key insight: the user's literal question is rarely a good search query. Reformulating it into direct-answer, background, recent-developments, counterargument, and code-example facets dramatically improves coverage.
- **Deduplication**: URLs deduplicated before scraping; same-domain results clustered.

### OpenCode

- Delegates to any configured MCP search tool (Brave, Exa, Tavily are interchangeable via config).
- Single-query-then-expand pattern: user query → search → LLM reads each result snippet and decides whether to fetch full content.
- Less structured than Claude Code's fan-out; more opportunistic. Useful for quick lookups, not deep synthesis.

### Cursor / Continue.dev

- `@web` context provider: fetches the top result from Bing and injects the raw text as context — no synthesis, no query diversification.
- Effectively a "lucky-first-result" shortcut. Fast but shallow.

### Exa Research API

- `POST /search` with `contents: true` returns search results WITH extracted page content in a single HTTP call — no separate scrape step.
- This is the cleanest single-provider path: one API key, one round-trip, high-quality extraction.
- Exa is optimized for LLM use: content is already cleaned and chunked.
- Brave Search + custom scrape is the general fallback.

### Key takeaways

1. **Query diversification by LLM** is what separates research from "just Google it". Non-negotiable.
2. **Provider abstraction** is necessary from day one — users have different API keys; Exa is the premium path, Brave/Tavily are the general paths.
3. **Content extraction quality matters more than quantity** — 5 well-extracted pages beat 20 snippets.
4. **Fan-out at the subagent level** is the correct long-term architecture. Goroutine-level fan-out is an equivalent interim: same concurrency shape, same interface, subagent dispatch slots in later.

---

## Architecture

### High-level flow

```
User: /research what are the best Go concurrency patterns?
                           │
                           ▼
              ┌─────────────────────────────┐
              │  ResearchOrchestrator.Run   │
              └─────────────────────────────┘
                           │
              ┌────────────▼────────────────┐
              │  QueryGenerator             │
              │  LLM → 4-5 diverse queries  │
              └─────────────────────────────┘
                           │
              ┌────────────▼────────────────┐
              │  Fan-out (goroutines)        │
              │  per query:                  │
              │    SearchProvider.Search     │
              │    → top K URLs              │
              │    → Scraper.Fetch(url)      │
              │    → ExtractText(html)       │
              └─────────────────────────────┘
                           │
              ┌────────────▼────────────────┐
              │  DeduplicateByURL + rank     │
              └─────────────────────────────┘
                           │
              ┌────────────▼────────────────┐
              │  Synthesizer                │
              │  LLM → Report + citations   │
              └─────────────────────────────┘
```

---

## `internal/research/provider.go` — search interface

```go
type SearchResult struct {
    Title   string
    URL     string
    Snippet string
    Content string // populated post-scrape, or by providers that return content (Exa)
}

type SearchProvider interface {
    Search(ctx context.Context, query string, maxResults int) ([]SearchResult, error)
    Name() string
}
```

---

## Providers

| Provider | When to use | Key env var |
|---|---|---|
| `ExaProvider` | Best quality — returns content alongside results | `EXA_API_KEY` |
| `BraveProvider` | Good general coverage; snippet-only, requires scrape | `BRAVE_SEARCH_API_KEY` |
| `TavilyProvider` | Research-focused; returns content optionally | `TAVILY_API_KEY` |
| `MCPSearchProvider` | Wraps any MCP tool whose schema matches search semantics | via MCP config |

**Resolution order**: explicit `research.provider` config key → env key scan (Exa → Tavily → Brave) → connected MCP search tool → error (no provider).

Provider lookup is done at `ResearchOrchestrator` construction time and fails loudly; no silent fallbacks at query time.

---

## `internal/research/scraper.go` — content extraction

### Approach

1. HTTP GET with a 10 s per-URL deadline (context-cancelled).
2. Parse `Content-Type`: if `text/html`, run the extraction pipeline; if `application/pdf` or other, skip (note in result).
3. Extraction pipeline:
   - Strip `<script>`, `<style>`, `<nav>`, `<header>`, `<footer>`, `<aside>` subtrees.
   - Run a port of Mozilla Readability (candidate library: `github.com/go-shiori/go-readability`) to extract the main article body.
   - Fallback: strip all remaining tags, normalize whitespace — always produces something.
4. Truncate to `MaxContentBytes` (default 50 000). Truncation is word-boundary-aware.
5. Token-bucket rate limiter: max 2 req/s per domain to avoid being blocked.

### `Scraper` interface

```go
type Scraper interface {
    Fetch(ctx context.Context, url string) (content string, err error)
}

// FetchAll runs Fetch concurrently across results, filling result.Content in place.
// Errors per-URL are logged, not fatal — a partial result set is returned.
func FetchAll(ctx context.Context, s Scraper, results []SearchResult) []SearchResult
```

---

## `internal/research/query_gen.go` — query diversification

```go
// GenerateQueries asks the LLM to produce N search queries that together
// cover the question across these facets:
//   (a) direct answer / definition
//   (b) background / fundamentals
//   (c) recent developments / news
//   (d) counterarguments / caveats
//   (e) practical examples / code
// Returns deduplicated []string, len 3..6.
func GenerateQueries(ctx context.Context, llm LLM, question string, n int) ([]string, error)
```

The LLM prompt is structured (JSON mode): `{ "queries": ["...", ...] }`. Query generation is the cheapest LLM call in the pipeline; a small/fast model is fine.

---

## `internal/research/orchestrator.go` — fan-out loop

```go
type Config struct {
    NumQueries      int // default 4
    ResultsPerQuery int // default 5
    MaxContentBytes int // default 50_000
}

type ResearchOrchestrator struct {
    cfg      Config
    provider SearchProvider
    scraper  Scraper
    llm      LLM
}

func (o *ResearchOrchestrator) Run(ctx context.Context, question string) (*Report, error) {
    queries, err := GenerateQueries(ctx, o.llm, question, o.cfg.NumQueries)
    if err != nil {
        return nil, err
    }

    var mu sync.Mutex
    var allResults []SearchResult
    var wg sync.WaitGroup
    errs := make([]error, len(queries))

    for i, q := range queries {
        wg.Add(1)
        go func(idx int, query string) {
            defer wg.Done()
            results, err := o.provider.Search(ctx, query, o.cfg.ResultsPerQuery)
            if err != nil {
                errs[idx] = err
                return
            }
            fetched := FetchAll(ctx, o.scraper, results)
            mu.Lock()
            allResults = append(allResults, fetched...)
            mu.Unlock()
        }(i, q)
    }
    wg.Wait()

    deduped := deduplicateByURL(allResults)
    return synthesize(ctx, o.llm, question, deduped)
}
```

**Subagent upgrade path**: when the subagent layer lands, the goroutine body becomes a subagent dispatch — same interface, no change to callers. The `LLM` interface is the coordination point.

---

## `internal/research/synthesizer.go` — synthesis

```go
type Citation struct {
    Num   int
    Title string
    URL   string
}

type Section struct {
    Heading string
    Body    string // may reference [N] citation markers
}

type Report struct {
    Summary   string
    Sections  []Section
    Citations []Citation
}
```

Single LLM call:
- **System**: "You are a research synthesizer. Answer the question accurately and concisely, citing sources as [N]. Provide a brief executive summary followed by titled sections."
- **User**: the original question + all source content, each prefixed `[N] Title\nURL\n\n<content>`.
- **Output format**: JSON `Report` struct (structured output / tool call mode).

Context window management: if total source content exceeds the model's context budget (estimated), chunk-summarize: split sources into groups, summarize each group, then synthesize from summaries. The chunking threshold is configurable (`research.context_budget`, default 128 000 tokens estimated).

---

## TUI integration

`/research <query>` dispatches to a `researchCmd` built-in (requires change #10 for slash dispatch):

1. Launches `ResearchOrchestrator.Run` in a goroutine.
2. Streams progress to TUI via a channel of `ResearchProgressMsg`:
   - `"Generating queries…"`
   - `"Searching [query 1 of 4]…"` (per goroutine start)
   - `"Scraping 18 pages…"`
   - `"Synthesizing…"`
3. Final `Report` is rendered as streaming markdown in the message thread, citations appended as a numbered list.
4. Cancellation: Ctrl-C sends context cancellation; in-flight goroutines drain within 10 s.

---

## Config additions (`internal/config/config.go`)

```go
type ResearchConfig struct {
    Provider       string // "exa" | "brave" | "tavily" | "mcp" | "" (auto)
    MaxQueries     int    // default 4
    MaxResults     int    // default 5
    MaxContentKB   int    // default 50
    ContextBudget  int    // default 128_000 (estimated tokens)
}
```

Exposed as `[research]` in `~/.fuse/config.yml`.

---

## New files

| Path | Purpose |
|---|---|
| `internal/research/provider.go` | `SearchProvider` interface + `SearchResult` |
| `internal/research/exa.go` | `ExaProvider` — Exa.ai search API |
| `internal/research/brave.go` | `BraveProvider` — Brave Search API |
| `internal/research/tavily.go` | `TavilyProvider` — Tavily API |
| `internal/research/mcp_provider.go` | `MCPSearchProvider` — wraps MCP search tools |
| `internal/research/scraper.go` | `Scraper` interface + HTTP fetch + readability extraction |
| `internal/research/query_gen.go` | `GenerateQueries` — LLM query diversification |
| `internal/research/orchestrator.go` | `ResearchOrchestrator` — fan-out loop |
| `internal/research/synthesizer.go` | `synthesize` + `Report`/`Citation`/`Section` types |
| `internal/tui/research_cmd.go` | `researchCmd` built-in, progress streaming |

## Modified files

| Path | Change |
|---|---|
| `internal/tui/builtin_provider.go` | Register `/research` (requires change #10) |
| `internal/config/config.go` | Add `ResearchConfig` + `[research]` YAML block |

---

## Open questions

1. **Subagent integration**: The goroutine fan-out is the first-pass shape. When the subagent architecture change lands (docket id TBD), each `query→search→scrape` unit becomes a proper subagent call, enabling per-step model selection, cost tracking, and cancellation signalling. The `ResearchOrchestrator` interface is stable regardless.

2. **robots.txt compliance**: Should Fuse respect `robots.txt` during scraping? Proposed: always-on for production, overridable via `research.scrape_polite: false` for development/testing.

3. **Context window overflow**: The chunk-summarize fallback above is the safety net. Should we proactively summarize sources above a per-source token threshold even within budget, to give the synthesizer more uniform inputs?

4. **Local result cache**: Cache search results to SQLite (keyed by `hash(provider+query)`, TTL 24h) to avoid re-fetching during development. Enabled via `research.cache: true`, disabled by default.
