# fuse

fuse is a terminal-based, multi-model AI agent harness: an interactive shell
that drives LLMs (via a LiteLLM gateway) with tools, a hardened subagent
runtime, human-in-the-loop permissions, MCP integration, and an embedded skill
system.

## Building

```sh
make build      # build the fuse binary
make install    # install to your GOPATH/bin
go test ./...   # run the test suite
```

Configuration lives in `~/.fuse/config.yml` (with an optional per-repo
`.fuse.local.yml` override). See `internal/config` for the full schema.

## Research

`/research <query>` produces a cited research report. It diversifies the
question into several facets, fans out one subagent per facet to search the web
and fetch the most promising pages, deduplicates sources, and synthesizes a
single markdown report with `[N]` citation markers and a numbered source list.
The flow is driven by the existing subagent runtime and an embedded `research`
skill; a user skill named `research` shadows the built-in one.

Two built-in tools back the flow: `web_search` (query a search provider) and
`web_fetch` (fetch a URL and extract the main article text, with a robots.txt
gate on by default).

### Search providers

You bring your own search API key. Provider resolution happens at first use and
fails loudly if nothing is configured, in this order:

1. an explicit `research.provider` in config (`brave` | `tavily` | `custom`)
2. the `BRAVE_SEARCH_API_KEY` environment variable
3. the `TAVILY_API_KEY` environment variable
4. a configured `[research.custom]` block
5. otherwise, a clear error naming all of the setup paths above

**Brave Search (primary, recommended).** Set `BRAVE_SEARCH_API_KEY`. Sign up at
api-dashboard.search.brave.com (a card is required; roughly $5/month of free
credits). fuse uses the Brave Search engine that also powers the web search in
some other AI assistants; here you supply your own key directly. This README's
Brave Search mention also serves as the attribution Brave's free credits ask
for.

**Tavily (alternative).** Set `TAVILY_API_KEY`. Tavily returns extracted page
content alongside results, which lets the research flow skip a separate fetch
for sources whose returned content already suffices.

**Custom / self-hosted (e.g. SearXNG).** Configure `[research.custom]` with a
URL template. For a SearXNG instance only the `url` is needed - the JSON field
mappings default to SearXNG's `format=json` response shape:

```yaml
research:
  custom:
    url: "https://searx.example.com/search?q={query}&format=json"
```

### The `[research]` config block

```yaml
research:
  provider: ""          # "" = auto (Brave env -> Tavily env -> custom); or brave | tavily | custom
  max_queries: 5        # facet count hint for the research flow
  max_results: 5        # results per web_search call
  max_content_kb: 50    # web_fetch content truncation (KB, word-boundary)
  respect_robots: true  # honor robots.txt in web_fetch; set false for dev/testing
  custom:
    url: ""             # template with {query} / {count}; empty = not configured
    headers: {}         # optional request headers
    results_path: results   # dotted path to the results array (SearXNG default)
    title_field: title      # per-result field mappings (SearXNG defaults)
    url_field: url
    snippet_field: content
```

### Observing the research flow — `research-probe`

The research flow is emergent and prompt-driven: the model diversifies a
question into facets, fans out one subagent per facet, and each child searches
and fetches on its own. That is hard to watch in the interactive shell (it
scrolls past) and unreachable from one-shot `fuse "<task>"` mode (which never
loads a skill slash command). `research-probe` runs the **real** flow — the
embedded `research` skill, the real `web_search`/`web_fetch` tools against your
configured provider, the real `spawn_agent` fan-out, talking to the live
gateway — headless and fully recorded, then prints an inspectable digest:

```sh
export TAVILY_API_KEY=...        # or BRAVE_SEARCH_API_KEY
fuse research-probe --model kimi "What is Litestream and how does it back up SQLite?"
```

It prints the spawn tree (root + one node per facet, with status), the census
of searches and fetches, every unique search query and fetched URL, and the
root's final synthesized report — or a clear "did not synthesize a report" line
when the flow fans out but never converges. Flags: `--model <alias>` picks the
driver model, `--trace <file>` also writes the raw gateway request/response
JSON, and `--timeout <dur>` bounds the whole run (default 3m). Nothing about the
agents is faked — the recorder is just an `agent.Renderer` layered over the
production wiring, so what you see is exactly what `/research` does.
