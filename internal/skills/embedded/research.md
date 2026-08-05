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

Spawn ONE subagent per facet in a SINGLE parallel batch via spawn_agent. Issue
all the spawn_agent calls together so the children run concurrently - do not
spawn them one at a time.

Give each child the tools it needs, including web_search and web_fetch. Instruct
each child to:

- web_search its facet question.
- Pick the most promising results (favor primary, authoritative, and recent
  sources).
- web_fetch each chosen result to read the full content.
- Return per-facet findings as concise prose, and include the source URL for
  every claim.

Keep nested spawn depth at 1: children DO NOT spawn further subagents. Each child
does its own searching and fetching directly and returns.

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
