<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0014 — Research Mode — Web Search, Fetch & Cited Synthesis on the Subagent Runtime](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0014-research-mode.md)**
<!-- docket:backlink:end -->

# Implementation Plan — Research Mode (change 0014)

Spec: `docs/superpowers/specs/2026-08-05-research-mode-design.md` (on the `docket` branch).
This plan executes on `feat/research-mode`, cut from `origin/main`.

> **Plan authored inline (auto fallback).** The configured plan skill
> `superpowers:writing-plans` is not installed on this machine; per docket's
> missing-skill rule the plan role degraded to `auto` and this file was authored
> directly. Same for the build/review/finish roles downstream — see the PR body.

## Grounding (verified against `origin/main` at plan time)

- Tool interface: `Name()/Description()/Parameters() map[string]any/Execute(ctx, args string) Result`
  (`internal/tools/registry.go`); `Result{Output string, IsError bool}`. Built-in template:
  `internal/tools/read.go` (JSON-tagged args struct, `json.Unmarshal`, error Results not panics).
  Registration: `DefaultTools()` in `registry.go`. `Registry.Subset` force-includes `spawn_agent`.
- Central spill truncation already wraps every tool result in `Registry.Execute`
  (`SpillOutput`) — tools return full text; no per-tool caps.
- Config: YAML, `internal/config/schema.go` — add a struct with `yaml:` tags, a field on
  `Config`, a mirror field on `rawConfig`, and defaults in `Default()`. Template: `PermissionsConfig`.
- Skills: `internal/skills/loader.go` `Load(dirs)` scans `<dir>/<name>/SKILL.md`, first-wins by
  name; `ParseSkill(path, data)` in `parser.go`; frontmatter needs `name:`, optional
  `slash_command:`/`description:`. **No `go:embed` path exists** — this plan adds one.
- Slash dispatch: `KindSkill` entries inject the skill body via `startPrompt`, appending
  `ARGUMENTS: <args>` (`internal/tui/shell_model.go:720`). A skill with `slash_command: /research`
  is dispatched through this exact path — **no bespoke `/research` builtin is needed** (spec
  simplification adopted at plan time; noted in Task 8).
- HTTP discipline to mirror: `internal/model/adapter.go` (per-attempt timeout, response-header
  timeout, bounded attempts + backoff, cancel-aware, labeled trace) — learning
  `bound-every-model-call`. No `Retry-After` parsing exists; this plan builds it.
- Sanitization is applied at TUI display (`sanitizeDisplay`, `internal/tui/renderer.go`); tools
  return plain text and the display path sanitizes — learning `sanitize-untrusted-bytes...`.
- Test conventions: `httptest.NewServer`; mutex-guard shared test doubles
  (`mutex-test-double-concurrent-provider`). YAML free-text scalars: extract via line reader,
  not raw unmarshal, when a `: ` can appear (`yaml-plain-scalar-colon-space`) — relevant only if
  a config value can carry `: ` unquoted; standard string fields are safe.

## Execution order & TDD

Each task is test-first: write the focused failing test, implement to green, self-review, one
commit. Later tasks depend on earlier ones. Run `go build ./... && go test ./...` before the
final task's commit as the whole-suite gate.

---

### Task 1 — `SearchProvider` interface + shared bounded HTTP helper

**Files:** `internal/research/provider.go`, `internal/research/http.go` (+ `_test.go`).

- `SearchResult{Title, URL, Snippet string}`; `SearchProvider{ Search(ctx, query string, maxResults int) ([]SearchResult, error); Name() string }`.
- `http.go`: a small bounded doer mirroring `adapter.go` — per-attempt timeout (10s default for
  search/fetch), bounded retries (3) on 429/5xx, **honor `Retry-After`** (delta-seconds or HTTP
  date; cap the wait), context-cancel-aware, and a labeled trace hook (`io.Writer`, mutex-guarded
  when shared). Errors carry provider/attempt/duration context.

**Tests:** `httptest` — success; 429 with `Retry-After: 1` waits then succeeds; 5xx retry
exhaustion returns a contextful error; context cancellation aborts mid-retry. Trace double is
mutex-guarded.

---

### Task 2 — `BraveSearchProvider`

**Files:** `internal/research/brave.go` (+ `_test.go`).

- `GET https://api.search.brave.com/res/v1/web/search?q=&count=` with `X-Subscription-Token`.
  Parse `web.results[]` → `[]SearchResult` (Title/URL/Snippet from `title`/`url`/`description`).
- Uses the Task-1 helper; base URL injectable for tests.

**Tests:** happy path (Brave-shaped JSON fixture); malformed JSON → error; 429/`Retry-After`
honored (via helper); empty results OK.

---

### Task 3 — `TavilyProvider`

**Files:** `internal/research/tavily.go` (+ `_test.go`).

- `POST https://api.tavily.com/search`, `Authorization: Bearer $TAVILY_API_KEY`,
  body `{query, max_results}`. Map `results[]` → `SearchResult`; when a result carries extracted
  `content`, seed `Snippet` with it (lets the skill skip a fetch).

**Tests:** happy path with content-bearing results; content seeds Snippet; malformed JSON; retry
behavior via helper.

---

### Task 4 — `CustomHTTPProvider` (user-land extension point)

**Files:** `internal/research/custom.go` (+ `_test.go`).

- Config-driven: `url` template with `{query}`/`{count}`, optional `headers`, JSON field maps
  `results_path`/`title_field`/`url_field`/`snippet_field` defaulting to SearXNG's shape
  (`results` / `title` / `url` / `content`). GET, JSON only.

**Tests:** SearXNG-shaped fixture with default mappings; overridden field mappings; unconfigured
(`url==""`) reports not-configured; `{query}`/`{count}` substitution + escaping.

---

### Task 5 — provider resolution

**Files:** `internal/research/resolve.go` (+ `_test.go`).

- `Resolve(cfg ResearchConfig, env) (SearchProvider, error)`: explicit `provider` config →
  `BRAVE_SEARCH_API_KEY` → `TAVILY_API_KEY` → `[research.custom]` if `url` set → loud error naming
  every setup path. Resolves at first use; no silent query-time fallback.

**Tests:** config beats env; Brave env beats Tavily env; custom last; nothing set → error names all
paths; explicit `provider: brave` with no key → error.

---

### Task 6 — scraper (`web_fetch` engine)

**Files:** `internal/research/scraper.go` (+ `_test.go`). Adds dep `github.com/go-shiori/go-readability`.

- HTTP GET, 10s deadline (Task-1 helper); robots.txt gate per host (fetch once, cache per
  scraper instance), `RespectRobots` toggle. `text/html` → strip script/style/nav/header/footer/
  aside → go-readability main-body extraction → tag-strip fallback (always yields something).
  Non-HTML (by content-type) skipped with a note. Word-boundary truncation at `MaxContentKB`
  (default 50). Token-bucket rate limit 2 req/s per domain.

**Tests:** fixture HTML → extracted body; robots-disallowed refusal; `RespectRobots:false` override
fetches; non-HTML skip note; truncation at word boundary; per-domain rate limiter paces (fake
clock or timing tolerance).

---

### Task 7 — `web_search` + `web_fetch` built-in tools

**Files:** `internal/tools/web_search.go`, `internal/tools/web_fetch.go` (+ `_test.go`);
register in `DefaultTools()`.

- `web_search`: args `{query, max_results?}`; resolves provider (Task 5) once (lazily, cached);
  returns titled results with URLs + snippets as text. `web_fetch`: args `{url}`; runs the scraper;
  returns extracted text or the skip/robots note. Both return plain text (display path sanitizes;
  spill wraps centrally). Error Results, never panics.

**Tests:** registration present in `DefaultTools()`; `Subset(["web_search","web_fetch"])` includes
both (+ forced `spawn_agent`); bad-args → error Result; provider/scraper injected via a seam for
deterministic tests.

---

### Task 8 — embedded `research` skill + loader embed path

**Files:** `skills/research.md` (source, `go:embed`), `internal/skills/embed.go` (+ `_test.go`),
extend `internal/skills/loader.go`.

- `embed.go`: `//go:embed research.md` → `Embedded() ([]Skill, error)` via `ParseSkill`. Add
  `LoadWithEmbedded(dirs)` (or extend `Load`) that runs filesystem discovery **first**, then folds
  in embedded skills only when the name is unseen — embedded ranks **lowest**, so a user
  `research` skill shadows it (first-wins preserved). Wire the TUI skill load
  (`SkillProvider`/`skills.Load` call site) to the embedded-aware entry point.
- `skills/research.md` frontmatter: `name: research`, `slash_command: /research`,
  `description: …`, body = the flow (4–5 facets → one `spawn_agent` per facet in a single parallel
  batch, each child `web_search`→`web_fetch`→return findings with URLs → dedup by URL → one cited
  synthesis with `[N]` markers + numbered source list). Note depth stays 1; slot-yield applies.

**`/research` dispatch:** realized by the embedded skill's `slash_command: /research` through the
existing `KindSkill` path — the query arrives as `ARGUMENTS:`. No new builtin in
`builtin_provider.go` (spec's "thin slash builtin" folded into the skill; simpler, reuses tested
dispatch). If the embedded skill fails to register, `/research` is simply absent — fail-visible.

**Tests:** `Embedded()` parses the skill; `LoadWithEmbedded` returns `research` when no user skill;
a user `research` skill shadows the embedded one; `SlashCommands()` maps `/research`.

---

### Task 9 — `ResearchConfig` wiring

**Files:** `internal/config/schema.go` (+ loader mirror), `_test.go`.

- Add `ResearchConfig{Provider string; MaxQueries, MaxResults, MaxContentKB int; RespectRobots bool;
  Custom CustomProviderConfig}` and `CustomProviderConfig{URL string; Headers map[string]string;
  ResultsPath, TitleField, URLField, SnippetField string}` — all `yaml:`-tagged. Field on `Config`
  + mirror on `rawConfig`; `Default()` sets MaxQueries 5, MaxResults 5, MaxContentKB 50,
  RespectRobots true, and the SearXNG-shaped custom field defaults. Providers/scraper read from
  this (thread the config to Task 5/6/7 seams).

**Tests:** YAML round-trip of a `research:` block; defaults when absent; `research.custom` parses;
`RespectRobots` default true.

---

### Task 10 — README + whole-suite gate

**Files:** `README.md`.

- `[research]` config block docs; Brave Search **attribution + key setup** note
  (api-dashboard.search.brave.com, BYO `BRAVE_SEARCH_API_KEY`); Tavily env; SearXNG-via-custom
  example (only the `url` needed). One-line `/research <query>` usage.
- Final gate: `go build ./... && go test ./...` green before commit.

---

## Out of scope (from spec)

Exa/MCP-search adapters; a dedicated SearXNG adapter; any zero-config keyless path; any new
orchestration/fan-out or Go query-gen/synthesis; PDF/non-HTML extraction; result cache; news
features; in-report editing/export.

## Build-time open question

Brave "LLM Context" endpoint possibly reducing fetch volume (spec D3/OQ1) — a quick spike only;
default to web-endpoint + readability unless it demonstrably wins. Not a blocker.
