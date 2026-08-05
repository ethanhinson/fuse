<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0014 — Research Mode — Web Search, Fetch & Cited Synthesis on the Subagent Runtime](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0014-research-mode.md)**
<!-- docket:backlink:end -->

# Research Mode — results

Change: #14 · Branch: feat/research-mode · PR: <set at PR open> · Plan: docs/superpowers/plans/2026-08-05-research-mode.md · ADRs: 2, 3, 4

## Verify (human)

The Go primitives (providers, HTTP client, resolution, scraper, tools, config,
embedded skill) are fully unit-tested. The end-to-end research flow is
prompt-enforced (skill-driven per ADR-0002), so its run shape cannot be
unit-tested and needs a manual pass at the merge gate:

- [ ] Set `BRAVE_SEARCH_API_KEY` (or `TAVILY_API_KEY`) and run `/research <a real
      question>` in the interactive shell; confirm it diversifies into ~4-5
      facets, spawns one subagent per facet (visible in the agent tree / Tab
      view), and returns a single cited markdown report with `[N]` markers and a
      numbered source list.
- [ ] Confirm `web_fetch` honors robots.txt by default and that
      `research.respect_robots: false` overrides it.
- [ ] Confirm the loud provider-setup error appears when no key/URL is configured
      (nothing set -> error naming BRAVE_SEARCH_API_KEY / TAVILY_API_KEY /
      [research.custom]).
- [ ] (Optional) Point `[research.custom]` at a SearXNG `format=json` URL and
      confirm the custom provider resolves.

## Findings

- Three architectural decisions were recorded as ADRs: ADR-0002 (research is
  skill-driven on the 0012 subagent runtime, zero new Go orchestration), ADR-0003
  (built-in skills ship via go:embed and rank below user skills so a user
  `research` skill shadows the built-in), ADR-0004 (BYO search key with
  Brave-primary provider resolution and a config-driven custom HTTP provider as
  the SearXNG extension point).
- A real correctness bug in the bounded HTTP retry loop was found and fixed
  (commit b72990c): `internal/research/http.go`'s `doOnce` cloned the request per
  attempt but did not rewind the body from `req.GetBody()`, so a retried POST
  (Tavily) sent an empty body on attempt 2+. `http.Client.Do` only auto-invokes
  GetBody for its own redirect handling, not for this caller-driven retry loop.
  Fixed by resetting `req.Body` from `GetBody` before each attempt; a regression
  test asserts both requests carry the full body.

## Follow-ups

- **Scraper robots-cache lock contention (non-blocking, from code review).**
  `internal/research/scraper.go` `robotsFor` holds the robots-cache mutex while
  performing the robots.txt HTTP GET, so the first request to a new host stalls
  other goroutines' cache checks until that fetch completes. Not a data race
  (passes -race) and not a correctness bug — a concurrency efficiency issue.
  Recommended fix: check cache -> release lock -> fetch -> re-acquire -> store.
  (Auto-capture is disabled in this repo, so this is reported here rather than
  minted as a stub.)
- **go-readability deprecation.** `github.com/go-shiori/go-readability` prints a
  deprecation notice pointing at `codeberg.org/readeck/go-readability/v2`; a
  future change may want to migrate.
- **Brave "LLM Context" endpoint (spec open question).** Evaluate whether Brave's
  AI-optimized context endpoint can reduce `web_fetch` volume; deferred - the web
  endpoint + readability extraction is the shipped default.

## Notable plan deviations

- No `README.md` existed in the repo; Task 10 created one from scratch (with the
  research section and the required Brave attribution) rather than editing an
  existing file.
- The `/research` slash command is realized by the embedded skill's own
  `slash_command: /research` (dispatched via the existing KindSkill body-injection
  path), so no bespoke `/research` builtin was added to `builtin_provider.go` -
  simpler and reuses the tested dispatch path.
- An earlier interleaved run left a stale "HALTED (concurrent writer)" entry in
  this change's reconcile log; that collision was resolved in this run (the
  duplicate plan file was removed and all plan tasks were driven to completion by
  the single active implementer). The stale entry is retained as honest audit
  history.
