---
name: research
slash_command: /research
description: Web research - diversify a question into facets, fan out subagents to search+fetch, and synthesize a cited report.
---
# Research

You are running a fan-out web research flow. The user's question arrives as
ARGUMENTS. Produce ONE cited markdown report by decomposing the question,
searching in parallel, and synthesizing the findings.

## Step 1 - Reformulate into facets

Read ARGUMENTS and rewrite the question into 4-5 distinct research facets. Cover
these angles:

1. Direct answer - the core question stated plainly.
2. Background - foundational context, definitions, and history.
3. Recent developments - the latest news, releases, or changes.
4. Counterarguments and caveats - objections, limitations, and risks.
5. Practical examples - concrete cases, applications, or how-to specifics.

Each facet must be a self-contained search-ready question. Skip a facet only
when it is genuinely irrelevant to ARGUMENTS.

## Step 2 - Fan out one subagent per facet

Spawn ONE `facet-researcher` worker per facet in a SINGLE parallel batch via
spawn_agent, passing `worker: "facet-researcher"`. Issue all the spawn_agent
calls together so the children run concurrently - do not spawn them one at a
time.

The `facet-researcher` worker already carries exactly the tools a facet needs
(web_search, web_fetch, read_file) and, by design, cannot itself spawn further
subagents - so nesting and per-child tool assembly are handled for you by the
workflow, not by instructions a child might ignore. Instruct each child to:

- web_search its facet question.
- Pick the most promising results (favor primary, authoritative, and recent
  sources).
- web_fetch each chosen result to read the full content.
- Return per-facet findings as concise prose, and include the source URL for
  every claim.

If spawn_agent is not among your tools this turn, do the facet work directly
yourself - search, fetch, and synthesize - rather than waiting on a spawn that
the workflow pool has (correctly) withheld.

### Spawn budget - read it, never count it yourself

Every spawn_agent result ends with a machine-generated line like
`agent budget: 7/16 used (9 remaining)`. This count is authored by the runtime -
trust it; do not tally your own spawns. Use it to decide whether to spawn again:

- One subagent per facet is enough. After the initial batch (4-5 children), you
  should NOT need to spawn more.
- When the budget line shows only a few remaining, STOP spawning immediately and
  move to synthesis with the findings you already have.
- If a spawn is refused with a "spawn budget exhausted" error, do not retry -
  proceed straight to Step 4 and synthesize from what returned.

## Step 3 - Deduplicate sources

Collect the findings from all facets. Deduplicate sources by URL across facets:
if two facets cite the same URL, keep one entry and merge what each contributed.

## Step 4 - Synthesize one report

Write ONE markdown report with:

- An executive summary at the top.
- Titled sections, one per major theme (usually aligned to the facets).
- Inline [N] citation markers wherever a claim rests on a source.
- A numbered source list at the end, where entry N is the URL (and title) for
  citation marker [N].

Ground every non-obvious claim in a cited source. Do not invent URLs. If a facet
returned nothing usable, say so plainly rather than filling the gap.

## Completion contract

Your reply is NOT complete until this final cited report exists. Producing the
[N]-cited markdown report with a numbered source list at the end IS the answer -
it is the last thing you do, after the subagents return. Do not stop at the
fan-out, and do not end your turn having only spawned agents or only collected
findings: always finish by writing the synthesized, cited report yourself.

Two elements are MANDATORY and non-negotiable in that final report:

1. Inline `[N]` numeric citation markers (e.g. `[1]`, `[2]`, `[3]`) attached to
   the claims they support. Use bracketed numbers - NOT inline markdown links
   like `[title](url)` - as the in-text citation form. You MAY additionally
   include links, but the `[N]` markers are required.
2. A numbered source list at the very END of the report, under a `## Sources`
   heading, where entry N gives the URL (and title) for citation marker `[N]`.
   Every `[N]` used in the body must have a matching numbered entry, and this
   list is the LAST thing in the report - do not omit it or stop before it.
