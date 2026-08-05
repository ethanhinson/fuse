<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0014 — Research Mode — Web Search, Fetch & Cited Synthesis on the Subagent Runtime](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0014-research-mode.md)**
<!-- docket:backlink:end -->

# Research Mode — Implementation Plan (change 0014)

Spec: `docs/superpowers/specs/2026-08-05-research-mode-design.md` (read from `docket` branch).
Module: `github.com/ethanhinson/fuse`, Go 1.26.5.

This plan builds the salvaged remainder of killed change 0011 on top of the 0012 subagent
runtime: two Go tools (`web_search`, `web_fetch`), a search-provider layer (Brave primary,
Tavily, custom HTTP), an embedded `research` skill, a `[research]` config block, and README
attribution. **No new Go orchestration/fan-out/synthesis** — the model + 0012 runtime own the flow.

## Reconcile deltas folded in (from the change's Reconcile log)

1. **Embedded skill needs a `go:embed` path** — `internal/skills` is filesystem-discovery only
   today. Add a lowest-precedence embedded source so a user `research` skill still shadows it.
2. **Config is YAML** (not TOML) — plug `ResearchConfig` into `internal/config/schema.go` mirroring
   the `PermissionsConfig`/`MCPServerConfig` pattern (`Config` + `rawConfig` + `mergeFile`).
3. **No `Retry-After` parsing exists** — the research HTTP layer builds bounded retry with
   `Retry-After` honoring fresh, mirroring the `bound-every-model-call` learning discipline.

## Learnings applied

- `bound-every-model-call`: every HTTP call in `internal/research` goes through one bounded client
  (per-attempt timeout, response-header timeout, bounded retries + backoff, cancel-aware, honoring
  `Retry-After` on 429/5xx). No `http.DefaultClient`.
- `mutex-test-double-concurrent-provider`: any test double shared with goroutines mutex-guards BOTH
  getter and setter.
- `sanitize-untrusted-bytes-fixed-width-tui`: fetched web bytes are hostile input — tool output
  rides the existing `sanitizeDisplay` path (already applied centrally by the node/renderer on
  ToolResult); the tools return plain extracted text, never raw HTML/control bytes to the model.
- `slot-cap-yield-while-blocked-on-children`: the fan-out uses the 0012 runtime's existing
  YieldSlot/UnyieldSlot; the skill directs one parallel batch, depth stays 1.
- `dirent-isdir-skips-symlinks`: N/A here (no new filesystem directory walking beyond the existing
  loader, which already handles it).

## Architecture decisions to record as ADRs (step 6)

- **ADR-A**: Research is skill-driven on the 0012 runtime — zero new Go orchestration; the model
  fans out via `spawn_agent`. (Spec D1.)
- **ADR-B**: First `go:embed` built-in skill + lowest-precedence merge so user skills shadow it.
  (New cross-cutting mechanism the spec's D2 requires but that did not exist before.)
- **ADR-C**: BYO search key, Brave-primary provider resolution order, custom HTTP provider as the
  user-land extension point instead of a per-engine SearXNG adapter. (Spec D3/D6.)

(The build agent should flag any *additional* non-obvious decision it makes; ADRs are minted in step 6.)

---

## Task breakdown (TDD — each task: failing test → implement → green → self-review → commit)

### Task 1 — `SearchProvider` interface + `SearchResult` (`internal/research/provider.go`)

- Create package `internal/research`.
- `SearchResult{ Title, URL, Snippet string }`.
- `SearchProvider interface { Search(ctx, query string, maxResults int) ([]SearchResult, error); Name() string }`.
- Tests: interface compile check + a trivial fake implementing it (used by later resolution tests).
- Commit: `feat(0014): research provider interface`.

### Task 2 — Bounded HTTP client for research (`internal/research/httpclient.go`)

- A shared `*http.Client` with `net.Dialer` timeout, `TLSHandshakeTimeout`, `ResponseHeaderTimeout`
  (mirror `internal/model/adapter.go`'s `defaultGatewayClient` shape but with shorter tool-scoped
  deadlines: e.g. dial 10s, header 15s).
- A `doWithRetry(ctx, req, maxAttempts, backoff)` helper: bounded retries on 429/5xx, honoring
  `Retry-After` (both delta-seconds and HTTP-date forms), cancel-aware, capped total wait, and rich
  error context (URL, attempt count, status, duration). This is the fresh `Retry-After` logic.
- Tests (`httptest`): success first try; 429 with `Retry-After: 1` then 200; 5xx retry exhaustion
  returns an error carrying attempt count; context cancel aborts mid-backoff.
- Commit: `feat(0014): bounded research HTTP client with Retry-After`.

### Task 3 — `BraveSearchProvider` (`internal/research/brave.go`)

- `GET https://api.search.brave.com/res/v1/web/search?q=...&count=N`, header
  `X-Subscription-Token: <key>`; parse `web.results[]` → `{title, url, description}` → `SearchResult`
  (snippet from `description`). Snippet-only (no page content).
- Uses the Task-2 client/retry. `Name() == "brave"`.
- Tests (`httptest`, base URL injectable): happy path maps fields; 429/`Retry-After` retried;
  malformed JSON → error; empty results → empty slice, no error.
- Note in code comment: Brave "LLM Context" endpoint evaluated and deferred (web endpoint +
  readability suffices) unless a quick spike shows otherwise — see spec open question 1.
- Commit: `feat(0014): Brave search provider`.

### Task 4 — `TavilyProvider` (`internal/research/tavily.go`)

- `POST https://api.tavily.com/search`, `Authorization: Bearer <key>`, body `{query, max_results}`;
  parse `results[]` → `{title, url, content}` → `SearchResult` (seed `Snippet` from returned
  `content` when present, so the skill can skip `web_fetch`). `Name() == "tavily"`.
- Same client/retry discipline.
- Tests (`httptest`): happy path incl. content→snippet; retry on 429; malformed JSON → error.
- Commit: `feat(0014): Tavily search provider`.

### Task 5 — `CustomHTTPProvider` (`internal/research/custom.go`)

- Config-driven (`CustomProviderConfig`): `URL` template with `{query}`/`{count}` placeholders,
  optional `Headers`, JSON field mappings `ResultsPath`/`TitleField`/`URLField`/`SnippetField`
  defaulting to SearXNG's `format=json` shape (`results`/`title`/`url`/`content`).
- GET only, JSON only, same client/retry. `Name() == "custom"`. A generic JSON walk resolves
  `ResultsPath` (dotted) then per-field extraction with the configured/default keys.
- Tests (`httptest`): SearXNG-shaped fixture with defaults; overridden field mappings; unconfigured
  block (empty URL) → treated as not-configured by resolution (Task 6).
- Commit: `feat(0014): config-driven custom HTTP search provider`.

### Task 6 — Provider resolution (`internal/research/resolve.go`)

- `ResolveProvider(cfg ResearchConfig, env func(string) string) (SearchProvider, error)`:
  explicit `cfg.Provider` (`brave`/`tavily`/`custom`) → `BRAVE_SEARCH_API_KEY` env →
  `TAVILY_API_KEY` env → `[research.custom]` if `URL` set → **loud error naming all setup paths**.
  Resolution happens at first use; no silent query-time fallback.
- Tests: config beats env; Brave env beats Tavily env; custom last; explicit provider with missing
  key → clear error; nothing configured → error naming BRAVE_SEARCH_API_KEY / TAVILY_API_KEY /
  `[research.custom]`.
- Commit: `feat(0014): search provider resolution order`.

### Task 7 — Scraper: fetch + robots + readability + rate limit (`internal/research/scraper.go`)

- Add `github.com/go-shiori/go-readability` to `go.mod` (`go get`), plus its deps; `go mod tidy`.
- `Fetch(ctx, url string, cfg ResearchConfig) (content string, err error)`:
  1. robots.txt gate per host (fetched once, cached per scraper instance; `RespectRobots` default
     true; `research.respect_robots: false` overrides) — disallowed URL returns a noted refusal.
  2. HTTP GET via the Task-2 client, 10s per-URL deadline.
  3. `text/html` → go-readability main-body extraction → tag-strip fallback (always yields text).
     Non-HTML (PDF etc.) skipped with a note.
  4. Word-boundary truncation at `MaxContentKB` (default 50).
  5. Token-bucket rate limit 2 req/s per domain.
- Tests: fixture HTML → extraction; robots-disallowed refusal; `respect_robots:false` honored;
  non-HTML skip note; truncation at word boundary; per-domain limiter paces (mutex-guarded double).
- Commit: `feat(0014): research scraper — fetch, robots, readability, rate limit`.

### Task 8 — `[research]` config block (`internal/config/schema.go` + `loader.go`)

- Add `ResearchConfig{ Provider string; MaxQueries, MaxResults, MaxContentKB int; RespectRobots bool;
  Custom CustomProviderConfig }` and `CustomProviderConfig{ URL; Headers; ResultsPath; TitleField;
  URLField; SnippetField }` with `yaml:` tags (`research`, nested `custom`).
- Add `Research ResearchConfig` to `Config` and `rawConfig`; defaults in `Default()`
  (MaxQueries 5, MaxResults 5, MaxContentKB 50, RespectRobots true); merge logic in `mergeFile`
  (respect the "0 means unset / keep default" convention used by the existing merges; RespectRobots
  needs an explicit presence check like SessionAllow).
- Tests: default values; YAML override of provider + nested custom block; robots override to false.
- Commit: `feat(0014): [research] config block`.

### Task 9 — `web_search` + `web_fetch` tools (`internal/tools/web_search.go`, `web_fetch.go`)

- `web_search`: params `{query, max_results?}`; resolves the provider from config (injected via a
  constructor `NewWebSearch(cfg)` — the tool holds the resolved-at-first-use provider), runs Search,
  formats results as a compact numbered list (title / url / snippet). Loud error surfaces provider
  setup guidance.
- `web_fetch`: params `{url}`; runs the scraper; returns extracted text (or the robots/non-HTML
  note). Both return `tools.Result`. Both implement the `tools.Tool` interface (match `bash.go`).
- Wire into `defaultToolRegistry` (`cmd/fuse/run.go`) so they register as ordinary built-ins →
  available to the main agent and, via `Registry.Subset`, to subagents (the research skill
  force-includes both by listing them in the child `tools`).
- Sanitization: rely on the central `sanitizeDisplay` on ToolResult; ensure tools never emit raw
  control bytes to the model (extracted text only).
- Tests: `web_search` registration + Name/Parameters + happy path against a fake provider;
  `web_fetch` against an `httptest` server (extraction + robots note); Subset inclusion when named.
- Commit: `feat(0014): web_search and web_fetch built-in tools`.

### Task 10 — Embedded `research` skill (`internal/skills` `go:embed` + `skills/research/SKILL.md`)

- Author `skills/research/SKILL.md` (repo-root `skills/` dir) with frontmatter
  (`name: research`, `description`, `slash_command: /research`) and the flow body per spec:
  diversify into 4–5 facets → one subagent per facet in a single parallel batch (each child:
  `web_search` → pick results → `web_fetch` → return per-facet findings WITH URLs) → dedup by URL
  → single cited synthesis (exec summary, titled sections, `[N]` markers, numbered sources) rendered
  as markdown. Children force-include `web_search`,`web_fetch` in their tool list; depth 1.
- Add `internal/skills/embedded.go`: `//go:embed research/SKILL.md` (or an `embedded/` subtree) →
  an `Embedded() []Skill` accessor that parses the embedded SKILL.md via `ParseSkill`.
- Merge embedded skills at LOWEST precedence in `skills.Load` (append after the disk dirs, guarded
  by the same `seen` name set so a user `research` skill shadows it) AND in
  `internal/tui/skill_provider.go`'s `reload()` (same lowest-precedence merge, so `/research`
  appears in the slash registry with the embedded body in its `expand()`).
- Tests: embedded skill parses; `skills.Load` includes it when no user skill; a user `research`
  skill shadows it (first-wins); SkillProvider surfaces `/research`.
- Commit: `feat(0014): embedded research skill via go:embed`.

### Task 11 — `/research` availability + guidance, and README attribution

- Confirm `/research` dispatches through the existing skill slash path (`handleSlashEntry` →
  `KindSkill` → body injection with `ARGUMENTS: <query>`); no new switch case needed because the
  embedded skill provides `/research`. Add a short validation note in the skill body directing a
  loud provider-setup error path when unconfigured (the model calls `web_search`, which surfaces the
  resolution error).
- README: add a **Brave Search attribution + key setup** section (BYO `BRAVE_SEARCH_API_KEY`,
  api-dashboard.search.brave.com signup, $5/mo free credits, Tavily alternative, custom/SearXNG via
  `[research.custom]` with the one-line URL example). Satisfies Brave free-credit attribution.
- Tests: none new for README; a small test asserting the embedded skill's slash command is
  `/research` (covered in Task 10). Manual verification of the end-to-end flow is expected at the
  merge gate (prompt-driven; see spec Testing).
- Commit: `feat(0014): /research wiring note + README Brave attribution`.

### Task 12 — Full-suite gate

- `go build ./...` and `go test ./...` green. `go vet ./...`. Confirm no `http.DefaultClient` usage
  in `internal/research`. Confirm `go mod tidy` left go.mod/go.sum consistent.
- Commit any tidy/format fixes: `chore(0014): go mod tidy + vet`.

## Out of scope (do not build)

- Exa / MCP-search adapters; a dedicated SearXNG adapter (custom HTTP covers it); any zero-config
  keyless default; any Go query-gen / synthesis / orchestration; PDF/non-HTML extraction; result
  cache; in-report editing/export; Brave LLM Context endpoint (evaluated, deferred).

## Verification notes

- The skill flow is prompt-enforced (spec D1) — unit tests cover the Go primitives; the end-to-end
  facet fan-out + citation shape is verified manually via the agent-tree TUI at the merge gate.
- Feature branch cut from `origin/main` (0fc9bda); adds only plan + code (no docket metadata).
